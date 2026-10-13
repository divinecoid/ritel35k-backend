package rfid

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

const (
	PermView   = "rfid.view"
	PermManage = "rfid.manage"

	MaxBatch = 2000
)

type Service struct {
	tx  *tx.Manager
	now func() time.Time
}

func NewService(t *tx.Manager) *Service { return &Service{tx: t, now: time.Now} }

// canManage: a tag with no store (central warehouse) needs a company-wide grant.
func canManage(p *access.Principal, storePath string) bool {
	if storePath == "" {
		return p.Grants.Scope(PermManage).All
	}
	return p.Grants.Can(PermManage, storePath)
}

func normalizeAll(epcs []string) ([]string, error) {
	if len(epcs) == 0 {
		return nil, apperr.Validation(map[string]string{"epcs": "at least one tag is required"})
	}
	if len(epcs) > MaxBatch {
		return nil, apperr.Validation(map[string]string{"epcs": fmt.Sprintf("at most %d tags per request", MaxBatch)})
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(epcs))
	for _, e := range epcs {
		n, err := NormalizeEPC(e)
		if err != nil {
			return nil, apperr.Validation(map[string]string{"epcs": "invalid EPC: " + e})
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out) // stable lock order
	return out, nil
}

// ---- registration

type RegisterInput struct {
	VariantID uuid.UUID
	StoreID   *uuid.UUID // nil = central warehouse
	Status    string     // REGISTERED | IN_STOCK (default) | ON_SHELF
	Location  string
	EPCs      []string
}

type RegisterResult struct {
	Created    int      `json:"created"`
	Duplicates []string `json:"duplicates"` // already registered (to this or another product): left untouched
}

// Register attaches tags to a product variant — what happens at receiving or when labels are encoded.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*RegisterResult, error) {
	p := access.MustPrincipal(ctx)
	epcs, err := normalizeAll(in.EPCs)
	if err != nil {
		return nil, err
	}
	status := in.Status
	if status == "" {
		status = InStock
	}
	if status != Registered && status != InStock && status != OnShelf {
		return nil, apperr.Validation(map[string]string{"status": "REGISTERED, IN_STOCK or ON_SHELF"})
	}
	res := &RegisterResult{Duplicates: []string{}}
	err = s.tx.Do(ctx, func(q *gen.Queries) error {
		path := ""
		if in.StoreID != nil {
			st, err := q.GetStore(ctx, gen.GetStoreParams{ID: *in.StoreID, CompanyID: p.CompanyID})
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.NotFound("store")
			}
			if err != nil {
				return err
			}
			path = st.Path
		}
		if !canManage(p, path) {
			return apperr.Forbidden("not allowed to register tags here")
		}
		v, err := q.LookupVariantForSale(ctx, gen.LookupVariantForSaleParams{ID: in.VariantID, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Validation(map[string]string{"variant_id": "unknown variant"})
		}
		if err != nil {
			return err
		}
		if !v.IsRfidTracked {
			return apperr.Conflict("PRODUCT_NOT_RFID_TRACKED", v.ProductName+" is not marked as RFID-tracked; enable it on the product first")
		}
		for _, e := range epcs {
			id, _ := uuid.NewV7()
			_, err := q.InsertTag(ctx, gen.InsertTagParams{ID: id, CompanyID: p.CompanyID, Epc: e, VariantID: in.VariantID, Status: status, StoreID: in.StoreID, Location: in.Location, CreatedBy: &p.UserID})
			if errors.Is(err, pgx.ErrNoRows) {
				res.Duplicates = append(res.Duplicates, e)
				continue
			}
			if err != nil {
				return err
			}
			res.Created++
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: in.StoreID, Action: "RFID_TAGS_REGISTERED", Entity: "product_variant", EntityID: in.VariantID.String(),
			New: map[string]any{"sku": v.Sku, "status": status, "created": res.Created, "duplicates": len(res.Duplicates)}})
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ---- status changes and moves (all-or-nothing)

type ChangeInput struct {
	EPCs   []string
	Status string
	Reason string
}

type BatchProblem struct {
	NotFound []string          `json:"not_found,omitempty"`
	Invalid  map[string]string `json:"invalid,omitempty"` // epc → why
}

func (s *Service) loadForChange(ctx context.Context, q *gen.Queries, p *access.Principal, epcs []string) ([]gen.LockTagsByEPCRow, map[string]string, error) {
	rows, err := q.LockTagsByEPC(ctx, gen.LockTagsByEPCParams{CompanyID: p.CompanyID, Epcs: epcs})
	if err != nil {
		return nil, nil, err
	}
	paths := map[uuid.UUID]string{}
	found := map[string]bool{}
	for _, r := range rows {
		found[r.Epc] = true
		if r.StoreID != nil {
			if _, ok := paths[*r.StoreID]; !ok {
				st, err := q.GetStore(ctx, gen.GetStoreParams{ID: *r.StoreID, CompanyID: p.CompanyID})
				if err != nil {
					return nil, nil, err
				}
				paths[*r.StoreID] = st.Path
			}
		}
	}
	problems := map[string]string{}
	for _, e := range epcs {
		if !found[e] {
			problems[e] = "not found"
		}
	}
	for _, r := range rows {
		path := ""
		if r.StoreID != nil {
			path = paths[*r.StoreID]
		}
		if !canManage(p, path) {
			problems[r.Epc] = "not allowed for this store"
		}
	}
	return rows, problems, nil
}

func problem(m map[string]string) error {
	if len(m) == 0 {
		return nil
	}
	return apperr.Conflict("TAG_CHANGE_REJECTED", "Some tags cannot be changed; nothing was applied").WithDetails(m)
}

func (s *Service) ChangeStatus(ctx context.Context, in ChangeInput) (int, error) {
	p := access.MustPrincipal(ctx)
	epcs, err := normalizeAll(in.EPCs)
	if err != nil {
		return 0, err
	}
	reason := strings.TrimSpace(in.Reason)
	if (in.Status == Lost || in.Status == Damaged) && len(reason) < 3 {
		return 0, apperr.Validation(map[string]string{"reason": "say what happened (lost / damaged)"})
	}
	n := 0
	err = s.tx.Do(ctx, func(q *gen.Queries) error {
		rows, problems, err := s.loadForChange(ctx, q, p, epcs)
		if err != nil {
			return err
		}
		from := map[string]int{}
		for _, r := range rows {
			if _, bad := problems[r.Epc]; bad {
				continue
			}
			if !CanChange(r.Status, in.Status) {
				problems[r.Epc] = r.Status + " → " + in.Status + " is not allowed"
				continue
			}
			from[r.Status]++
		}
		if err := problem(problems); err != nil {
			return err
		}
		affected, err := q.SetTagsStatus(ctx, gen.SetTagsStatusParams{CompanyID: p.CompanyID, Epcs: epcs, Status: in.Status})
		if err != nil {
			return err
		}
		n = int(affected)
		return audit.Record(ctx, q, audit.Entry{Action: "RFID_TAGS_STATUS", Entity: "rfid_tag", EntityID: epcs[0],
			New: map[string]any{"to": in.Status, "count": n, "from": from, "reason": reason, "first_epc": epcs[0]}})
	})
	return n, err
}

type MoveInput struct {
	EPCs     []string
	StoreID  *uuid.UUID
	Location string
}

func (s *Service) Move(ctx context.Context, in MoveInput) (int, error) {
	p := access.MustPrincipal(ctx)
	epcs, err := normalizeAll(in.EPCs)
	if err != nil {
		return 0, err
	}
	n := 0
	err = s.tx.Do(ctx, func(q *gen.Queries) error {
		destPath := ""
		if in.StoreID != nil {
			st, err := q.GetStore(ctx, gen.GetStoreParams{ID: *in.StoreID, CompanyID: p.CompanyID})
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.NotFound("store")
			}
			if err != nil {
				return err
			}
			destPath = st.Path
		}
		if !canManage(p, destPath) {
			return apperr.Forbidden("not allowed to move tags into that store")
		}
		rows, problems, err := s.loadForChange(ctx, q, p, epcs)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.Status == Sold {
				problems[r.Epc] = "sold items cannot be moved"
			}
		}
		if err := problem(problems); err != nil {
			return err
		}
		affected, err := q.MoveTags(ctx, gen.MoveTagsParams{CompanyID: p.CompanyID, Epcs: epcs, StoreID: in.StoreID, Location: in.Location})
		if err != nil {
			return err
		}
		n = int(affected)
		return audit.Record(ctx, q, audit.Entry{StoreID: in.StoreID, Action: "RFID_TAGS_MOVED", Entity: "rfid_tag", EntityID: epcs[0],
			New: map[string]any{"count": n, "to_store": in.StoreID, "location": in.Location}})
	})
	return n, err
}

// ---- reads

type TagFilter struct {
	StoreID   *uuid.UUID
	VariantID *uuid.UUID
	UnderPath string
	Status    string
	Q         string
}

func (s *Service) List(ctx context.Context, f TagFilter, pg pagination.Params) ([]gen.ListTagsRow, pagination.Meta, error) {
	p := access.MustPrincipal(ctx)
	sc := p.Grants.Scope(PermView)
	if sc.Empty() {
		return nil, pagination.Meta{}, apperr.Forbidden("no access to RFID data")
	}
	all, paths := sc.SQL()
	q := s.tx.Queries()
	rows, err := q.ListTags(ctx, gen.ListTagsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, UnderPath: f.UnderPath, VariantID: f.VariantID,
		Status: f.Status, Q: f.Q, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	total, err := q.CountTags(ctx, gen.CountTagsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, UnderPath: f.UnderPath, VariantID: f.VariantID, Status: f.Status, Q: f.Q})
	return rows, pagination.NewMeta(pg, total), err
}

type TagDetail struct {
	gen.GetTagByEPCRow
	History []gen.TagHistoryRow `json:"history"`
}

func (s *Service) Get(ctx context.Context, epc string) (*TagDetail, error) {
	p := access.MustPrincipal(ctx)
	e, err := NormalizeEPC(epc)
	if err != nil {
		return nil, err
	}
	q := s.tx.Queries()
	t, err := q.GetTagByEPC(ctx, gen.GetTagByEPCParams{CompanyID: p.CompanyID, Epc: e})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("tag")
	}
	if err != nil {
		return nil, err
	}
	visible := (t.Path == "" && p.Grants.Scope(PermView).All) || (t.Path != "" && p.Grants.Can(PermView, t.Path))
	if !visible {
		return nil, apperr.NotFound("tag")
	}
	h, err := q.TagHistory(ctx, gen.TagHistoryParams{CompanyID: p.CompanyID, Epc: e})
	if err != nil {
		return nil, err
	}
	return &TagDetail{GetTagByEPCRow: t, History: h}, nil
}

// Variance compares tags recorded per store/SKU with the system stock balance.
func (s *Service) Variance(ctx context.Context, f TagFilter, onlyDiff bool, limit int) ([]gen.RFIDStockVarianceRow, error) {
	p := access.MustPrincipal(ctx)
	sc := p.Grants.Scope(PermView)
	if sc.Empty() {
		return nil, apperr.Forbidden("no access to RFID data")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	all, paths := sc.SQL()
	return s.tx.Queries().RFIDStockVariance(ctx, gen.RFIDStockVarianceParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, UnderPath: f.UnderPath, StoreID: f.StoreID, OnlyDiff: onlyDiff, RowLimit: int32(limit)})
}

// ---- readers

var readerCode = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{1,29}$`)

func newToken() (token, hash, hint string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token = "rdr_" + base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), token[len(token)-4:]
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

type ReaderInput struct {
	StoreID uuid.UUID
	Code    string
	Name    string
	Kind    string
}

type ReaderWithToken struct {
	Reader gen.RfidReader `json:"reader"`
	Token  string         `json:"token"` // shown once; only its hash is kept
}

func (s *Service) CreateReader(ctx context.Context, in ReaderInput) (*ReaderWithToken, error) {
	p := access.MustPrincipal(ctx)
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	if !readerCode.MatchString(code) {
		return nil, apperr.Validation(map[string]string{"code": "2-30 characters: letters, digits, - and _"})
	}
	var out ReaderWithToken
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		st, err := q.GetStore(ctx, gen.GetStoreParams{ID: in.StoreID, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("store")
		}
		if err != nil {
			return err
		}
		if !canManage(p, st.Path) {
			return apperr.Forbidden("not allowed to enroll readers in this store")
		}
		tok, hash, hint := newToken()
		id, _ := uuid.NewV7()
		r, err := q.CreateReader(ctx, gen.CreateReaderParams{ID: id, CompanyID: p.CompanyID, StoreID: in.StoreID, Code: code, Name: in.Name, Kind: in.Kind, TokenHash: hash, TokenHint: hint, CreatedBy: &p.UserID})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return apperr.Conflict("READER_EXISTS", "A reader with this code already exists in the store")
		}
		if err != nil {
			return err
		}
		out = ReaderWithToken{Reader: r, Token: tok}
		out.Reader.TokenHash = ""
		return audit.Record(ctx, q, audit.Entry{StoreID: &in.StoreID, Action: "RFID_READER_ENROLLED", Entity: "rfid_reader", EntityID: id.String(), New: map[string]any{"code": code, "kind": in.Kind}})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) readerFor(ctx context.Context, q *gen.Queries, p *access.Principal, id uuid.UUID) (gen.GetReaderRow, error) {
	r, err := q.GetReader(ctx, gen.GetReaderParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !canManage(p, r.Path)) {
		return r, apperr.NotFound("reader")
	}
	return r, err
}

func (s *Service) UpdateReader(ctx context.Context, id uuid.UUID, name, status *string) (*gen.RfidReader, error) {
	p := access.MustPrincipal(ctx)
	var out gen.RfidReader
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		r, err := s.readerFor(ctx, q, p, id)
		if err != nil {
			return err
		}
		out, err = q.UpdateReader(ctx, gen.UpdateReaderParams{ID: id, CompanyID: p.CompanyID, Name: name, Status: status})
		if err != nil {
			return err
		}
		out.TokenHash = ""
		return audit.Record(ctx, q, audit.Entry{StoreID: &r.StoreID, Action: "RFID_READER_UPDATED", Entity: "rfid_reader", EntityID: id.String(), New: map[string]any{"name": name, "status": status}})
	})
	return &out, err
}

// RotateToken issues a new credential; the old one stops working immediately.
func (s *Service) RotateToken(ctx context.Context, id uuid.UUID) (string, error) {
	p := access.MustPrincipal(ctx)
	var tok string
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		r, err := s.readerFor(ctx, q, p, id)
		if err != nil {
			return err
		}
		t, hash, hint := newToken()
		if err := q.RotateReaderToken(ctx, gen.RotateReaderTokenParams{ID: id, TokenHash: hash, TokenHint: hint}); err != nil {
			return err
		}
		tok = t
		return audit.Record(ctx, q, audit.Entry{StoreID: &r.StoreID, Action: "RFID_READER_TOKEN_ROTATED", Entity: "rfid_reader", EntityID: id.String()})
	})
	return tok, err
}

func (s *Service) Readers(ctx context.Context, storeID *uuid.UUID) ([]gen.ListReadersRow, error) {
	p := access.MustPrincipal(ctx)
	sc := p.Grants.Scope(PermView)
	if sc.Empty() {
		return nil, apperr.Forbidden("no access to RFID data")
	}
	all, paths := sc.SQL()
	return s.tx.Queries().ListReaders(ctx, gen.ListReadersParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: storeID})
}
