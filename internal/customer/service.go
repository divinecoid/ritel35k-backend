package customer

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/numbering"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

const (
	PermView   = "customer.view"
	PermCreate = "customer.create"
	PermManage = "customer.manage"
)

type Service struct {
	tx  *tx.Manager
	now func() time.Time
}

func NewService(t *tx.Manager) *Service { return &Service{tx: t, now: time.Now} }

// NormalizePhone keeps digits only and unifies Indonesian prefixes (+62 / 62 → 0), so "+62 812-3456" and
// "0812 3456" are the same customer.
func NormalizePhone(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if strings.HasPrefix(d, "62") {
		d = "0" + d[2:]
	}
	return d
}

type Input struct {
	Name      string
	Phone     string
	Email     string
	Birthday  *time.Time
	Gender    string
	Address   string
	Notes     string
	HomeStore *uuid.UUID
}

func (s *Service) Create(ctx context.Context, in Input) (*gen.Customer, error) {
	p := access.MustPrincipal(ctx)
	if !p.Grants.Has(PermCreate) && !p.Grants.Has(PermManage) {
		return nil, apperr.Forbidden("missing permission")
	}
	phone := NormalizePhone(in.Phone)
	if len(phone) < 8 || len(phone) > 15 {
		return nil, apperr.Validation(map[string]string{"phone": "8-15 digits"})
	}
	var out gen.Customer
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		lvl, err := q.GetDefaultLevel(ctx, p.CompanyID)
		if err != nil {
			return err
		}
		root, err := q.GetCompanyRootOrgUnit(ctx, p.CompanyID)
		if err != nil {
			return err
		}
		no, err := numbering.NextAt(ctx, q, numbering.Owner{CompanyID: p.CompanyID, ID: p.CompanyID, Code: "", OrgUnitID: root.ID, Timezone: "Asia/Jakarta"}, "member", s.now())
		if err != nil {
			return err
		}
		id, _ := uuid.NewV7()
		var bday *time.Time
		if in.Birthday != nil {
			t := time.Date(in.Birthday.Year(), in.Birthday.Month(), in.Birthday.Day(), 0, 0, 0, 0, time.UTC)
			bday = &t
		}
		out, err = q.CreateCustomer(ctx, gen.CreateCustomerParams{ID: id, CompanyID: p.CompanyID, MemberNo: no, Name: strings.TrimSpace(in.Name), Phone: phone, Email: strings.TrimSpace(in.Email),
			Birthday: bday, Gender: in.Gender, Address: in.Address, Notes: in.Notes, LevelID: lvl.ID, HomeStoreID: in.HomeStore, CreatedBy: &p.UserID})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return apperr.Conflict("PHONE_TAKEN", "A customer with this phone number already exists")
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: "CUSTOMER_CREATED", Entity: "customer", EntityID: id.String(), New: map[string]any{"member_no": no, "phone": phone}})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type Detail struct {
	gen.GetCustomerRow
	Ledger    []gen.LoyaltyTransaction `json:"ledger"`
	Sales     []gen.CustomerSalesRow   `json:"sales"`
	NextLevel *struct {
		Name      string          `json:"name"`
		MinSpend  decimal.Decimal `json:"min_spend"`
		Remaining decimal.Decimal `json:"remaining"`
	} `json:"next_level"`
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Detail, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	c, err := q.GetCustomer(ctx, gen.GetCustomerParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("customer")
	}
	if err != nil {
		return nil, err
	}
	led, err := q.ListLoyalty(ctx, gen.ListLoyaltyParams{CustomerID: id, Limit: 50})
	if err != nil {
		return nil, err
	}
	sales, err := q.CustomerSales(ctx, gen.CustomerSalesParams{CustomerID: &id, Limit: 20})
	if err != nil {
		return nil, err
	}
	d := &Detail{GetCustomerRow: c, Ledger: led, Sales: sales}
	if lv, err := q.ListLevels(ctx, p.CompanyID); err == nil {
		for _, l := range lv {
			if l.Rank > c.LevelRank {
				d.NextLevel = &struct {
					Name      string          `json:"name"`
					MinSpend  decimal.Decimal `json:"min_spend"`
					Remaining decimal.Decimal `json:"remaining"`
				}{l.Name, l.MinSpend, decimal.Max(l.MinSpend.Sub(c.LifetimeSpend), decimal.Zero)}
				break
			}
		}
	}
	return d, nil
}

type Filter struct {
	Q       string
	LevelID *uuid.UUID
	Status  string
}

func (s *Service) List(ctx context.Context, f Filter, pg pagination.Params) ([]gen.ListCustomersRow, pagination.Meta, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	term := f.Q
	if digits := NormalizePhone(f.Q); len(digits) >= 4 && digits == strings.TrimSpace(strings.TrimPrefix(f.Q, "+")) {
		term = digits // searching by phone
	}
	rows, err := q.ListCustomers(ctx, gen.ListCustomersParams{CompanyID: p.CompanyID, Q: term, LevelID: f.LevelID, Status: f.Status, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	total, err := q.CountCustomers(ctx, gen.CountCustomersParams{CompanyID: p.CompanyID, Q: term, LevelID: f.LevelID, Status: f.Status})
	return rows, pagination.NewMeta(pg, total), err
}

// Lookup finds one customer by exact phone or member number (the cashier's quick path).
func (s *Service) Lookup(ctx context.Context, key string) (*gen.GetCustomerRow, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	key = strings.TrimSpace(key)
	rows, err := q.ListCustomers(ctx, gen.ListCustomersParams{CompanyID: p.CompanyID, Q: NormalizePhoneOrKey(key), Status: "active", PageLimit: 2})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Phone == NormalizePhone(key) || strings.EqualFold(r.MemberNo, key) {
			c, err := q.GetCustomer(ctx, gen.GetCustomerParams{ID: r.ID, CompanyID: p.CompanyID})
			return &c, err
		}
	}
	return nil, apperr.NotFound("customer")
}

func NormalizePhoneOrKey(k string) string {
	if d := NormalizePhone(k); len(d) >= 8 {
		return d
	}
	return k
}

type UpdateInput struct {
	Name, Email, Gender, Address, Notes, Status *string
	Birthday                                    *time.Time
	LevelID                                     *uuid.UUID
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (*gen.Customer, error) {
	p := access.MustPrincipal(ctx)
	if !p.Grants.Has(PermManage) {
		return nil, apperr.Forbidden("missing permission")
	}
	var out gen.Customer
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		if in.LevelID != nil {
			if _, err := q.GetLevel(ctx, gen.GetLevelParams{ID: *in.LevelID, CompanyID: p.CompanyID}); err != nil {
				return apperr.Validation(map[string]string{"level_id": "unknown level"})
			}
		}
		c, err := q.UpdateCustomer(ctx, gen.UpdateCustomerParams{ID: id, CompanyID: p.CompanyID, Name: in.Name, Email: in.Email, Birthday: in.Birthday, Gender: in.Gender,
			Address: in.Address, Notes: in.Notes, Status: in.Status, LevelID: in.LevelID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("customer")
		}
		if err != nil {
			return err
		}
		out = c
		return audit.Record(ctx, q, audit.Entry{Action: "CUSTOMER_UPDATED", Entity: "customer", EntityID: id.String(), New: in})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AdjustPoints is a manual correction (goodwill, fixing a mistake). The balance can never go below zero.
func (s *Service) AdjustPoints(ctx context.Context, id uuid.UUID, points int64, note string) (int64, error) {
	p := access.MustPrincipal(ctx)
	if !p.Grants.Has(PermManage) {
		return 0, apperr.Forbidden("missing permission")
	}
	if points == 0 {
		return 0, apperr.Validation(map[string]string{"points": "must not be zero"})
	}
	var bal int64
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		c, err := q.GetCustomerForUpdate(ctx, gen.GetCustomerForUpdateParams{ID: id, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("customer")
		}
		if err != nil {
			return err
		}
		if c.PointsBalance+points < 0 {
			return apperr.Conflict("INSUFFICIENT_POINTS", "Adjustment would make the balance negative")
		}
		bal, err = post(ctx, q, p.CompanyID, id, KindAdjust, points, nil, nil, note, &p.UserID)
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: "POINTS_ADJUSTED", Entity: "customer", EntityID: id.String(), New: map[string]any{"points": points, "note": note}})
	})
	return bal, err
}

func (s *Service) Levels(ctx context.Context) ([]gen.MembershipLevel, error) {
	return s.tx.Queries().ListLevels(ctx, access.MustPrincipal(ctx).CompanyID)
}

func (s *Service) UpdateLevel(ctx context.Context, id uuid.UUID, name *string, minSpend, mult *decimal.Decimal) (*gen.MembershipLevel, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, PermManage); err != nil {
		return nil, err
	}
	l, err := s.tx.Queries().UpdateLevel(ctx, gen.UpdateLevelParams{ID: id, CompanyID: p.CompanyID, Name: name, MinSpend: minSpend, EarnMultiplier: mult})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("level")
	}
	return &l, err
}

// ExpirePoints removes points that were earned long enough ago to have expired and were not spent.
// It is idempotent: running it twice in a row expires nothing the second time. Run it from the worker.
func ExpirePoints(ctx context.Context, m *tx.Manager, now time.Time) (int, error) {
	rows, err := m.Queries().ExpirableCustomers(ctx, now)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		expirable := r.EarnedExpired - r.NetRedeemed - r.AlreadyExpired
		if expirable > r.PointsBalance {
			expirable = r.PointsBalance
		}
		if expirable <= 0 {
			continue
		}
		err := m.Do(ctx, func(q *gen.Queries) error {
			c, err := q.GetCustomerForUpdate(ctx, gen.GetCustomerForUpdateParams{ID: r.CustomerID, CompanyID: r.CompanyID})
			if err != nil {
				return err
			}
			take := expirable
			if take > c.PointsBalance {
				take = c.PointsBalance
			}
			if take <= 0 {
				return nil
			}
			_, err = post(ctx, q, r.CompanyID, r.CustomerID, KindExpire, -take, nil, nil, "points expired", nil)
			return err
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
