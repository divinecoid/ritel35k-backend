package rfid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/internal/notification"
	"github.com/ritel35k/backend/internal/platform/reqmeta"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/internal/security"
	"github.com/ritel35k/backend/internal/settings"
	"github.com/ritel35k/backend/pkg/apperr"
)

// Device is the edge-facing API: a reader authenticates with its own token (not a user session) and may only
// touch its own store. Everything here is idempotent so the edge can retry its buffer blindly.
type Device struct {
	httpx.Base
	tx  *tx.Manager
	now func() time.Time
}

func NewDevice(b httpx.Base, t *tx.Manager) *Device { return &Device{Base: b, tx: t, now: time.Now} }

type readerCtx struct{}

func readerFrom(ctx context.Context) gen.GetReaderByTokenRow {
	return ctx.Value(readerCtx{}).(gen.GetReaderByTokenRow)
}

// Auth resolves "Authorization: Bearer rdr_…" to an active reader of an active store.
func (d *Device) Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || !strings.HasPrefix(tok, "rdr_") || len(tok) > 100 {
			d.Fail(w, r, apperr.Unauthenticated("reader token required"))
			return
		}
		rd, err := d.tx.Queries().GetReaderByToken(r.Context(), hashToken(tok))
		if errors.Is(err, pgx.ErrNoRows) {
			d.Fail(w, r, apperr.Unauthenticated("unknown reader token"))
			return
		}
		if err != nil {
			d.Fail(w, r, err)
			return
		}
		if rd.Status != "active" {
			d.Fail(w, r, apperr.Forbidden("reader is disabled"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), readerCtx{}, rd)))
	})
}

// Mount adds the reader endpoints to the shared /edge router (other device APIs, e.g. POS terminals, live beside them).
func (d *Device) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(d.Auth)
		r.Post("/events", d.events)
		r.Get("/snapshot", d.snapshot)
		r.Post("/heartbeat", d.heartbeat)
	})
}

// StoreRules are the per-store RFID settings the edge needs; the server owns them ("store is data").
type StoreRules struct {
	SoldGraceSeconds   int  `json:"sold_grace_seconds"`
	ReportUnknown      bool `json:"report_unknown_tags"`
	LocalAlarmOnUnknow bool `json:"local_alarm_on_unknown"`
}

func (d *Device) rules(ctx context.Context, q *gen.Queries, rd gen.GetReaderByTokenRow) (StoreRules, error) {
	st, err := q.GetStore(ctx, gen.GetStoreParams{ID: rd.StoreID, CompanyID: rd.CompanyID})
	if err != nil {
		return StoreRules{}, err
	}
	if st.Status != "active" {
		return StoreRules{}, apperr.Conflict("STORE_INACTIVE", "Store is not active")
	}
	snap, err := settings.LoadSnapshot(ctx, q, rd.CompanyID, st.OrgUnitID)
	if err != nil {
		return StoreRules{}, err
	}
	return StoreRules{SoldGraceSeconds: int(snap.Decimal("rfid.sold_grace_seconds").IntPart()), ReportUnknown: snap.Bool("rfid.report_unknown_tags"),
		LocalAlarmOnUnknow: snap.Bool("rfid.local_alarm_on_unknown")}, nil
}

// ---- events

type EventIn struct {
	ClientEventID uuid.UUID `json:"client_event_id" validate:"required"`
	EPC           string    `json:"epc" validate:"required,max=80"`
	ReadAt        time.Time `json:"read_at"`
	Direction     string    `json:"direction" validate:"omitempty,oneof=in out unknown"`
	RSSI          *int16    `json:"rssi"`
	LocalDecision string    `json:"local_decision" validate:"omitempty,oneof=allow alarm"`
}

type eventsRequest struct {
	Events       []EventIn `json:"events" validate:"required,min=1,max=500,dive"`
	AgentVersion string    `json:"agent_version" validate:"max=40"`
}

type EventResult struct {
	ClientEventID   uuid.UUID  `json:"client_event_id"`
	Decision        string     `json:"decision"`
	Reason          string     `json:"reason"`
	Duplicate       bool       `json:"duplicate"`
	SecurityEventID *uuid.UUID `json:"security_event_id,omitempty"`
}

func (d *Device) events(w http.ResponseWriter, r *http.Request) {
	var req eventsRequest
	if err := httpx.Bind(r, &req); err != nil {
		d.Fail(w, r, err)
		return
	}
	rd := readerFrom(r.Context())
	results, err := d.ProcessEvents(r.Context(), rd, req.Events, reqmeta.From(r.Context()).IP, req.AgentVersion)
	if err != nil {
		d.Fail(w, r, err)
		return
	}
	response.OK(w, map[string]any{"results": results, "server_time": d.now().UTC()})
}

// ProcessEvents stores a batch and decides every gate read. It runs in one transaction: a retry after a failure
// either finds everything applied (duplicates) or nothing.
func (d *Device) ProcessEvents(ctx context.Context, rd gen.GetReaderByTokenRow, evs []EventIn, ip, version string) ([]EventResult, error) {
	now := d.now()
	results := make([]EventResult, 0, len(evs))
	err := d.tx.Do(ctx, func(q *gen.Queries) error {
		results = results[:0]
		if err := q.TouchReader(ctx, gen.TouchReaderParams{ID: rd.ID, Ip: ip, Version: version}); err != nil {
			return err
		}
		rules, err := d.rules(ctx, q, rd)
		if err != nil {
			return err
		}
		type ev struct {
			EventIn
			epc string
			bad bool
		}
		list := make([]ev, len(evs))
		epcs := make([]string, 0, len(evs))
		for i, e := range evs {
			n, nerr := NormalizeEPC(e.EPC)
			list[i] = ev{EventIn: e, epc: n, bad: nerr != nil}
			if e.ReadAt.IsZero() || e.ReadAt.After(now.Add(5*time.Minute)) {
				list[i].ReadAt = now // a reader with a wrong clock must not be able to backdate/forward-date decisions
			}
			if nerr == nil {
				epcs = append(epcs, n)
			}
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].ReadAt.Before(list[j].ReadAt) })
		tags := map[string]gen.GateLookupTagsRow{}
		if len(epcs) > 0 {
			rows, err := q.GateLookupTags(ctx, gen.GateLookupTagsParams{CompanyID: rd.CompanyID, Epcs: epcs})
			if err != nil {
				return err
			}
			for _, t := range rows {
				tags[t.Epc] = t
			}
		}
		gate := rd.Kind == "gate"
		for _, e := range list {
			res := EventResult{ClientEventID: e.ClientEventID}
			if e.bad {
				res.Decision, res.Reason = Ignore, "invalid epc"
				results = append(results, res)
				continue
			}
			dir := e.Direction
			if dir == "" {
				dir = "unknown"
			}
			dec := Decision{Result: Ignore, Reason: "not a gate reader"}
			var secID *uuid.UUID
			if gate {
				var facts *TagFacts
				var info *gen.GateLookupTagsRow
				if t, ok := tags[e.epc]; ok {
					facts = &TagFacts{Status: t.Status, StoreID: t.StoreID, SoldAt: t.SoldAt}
					info = &t
				}
				dec = DecideGate(facts, rd.StoreID, dir, e.ReadAt, Rules{SoldGrace: time.Duration(rules.SoldGraceSeconds) * time.Second, ReportUnknown: rules.ReportUnknown})
				if dec.Record {
					id, err := d.raise(ctx, q, rd, e.EventIn, e.epc, e.ReadAt, dec, info)
					if err != nil {
						return err
					}
					secID = id
				}
			}
			id, _ := uuid.NewV7()
			_, ierr := q.InsertRFIDEvent(ctx, gen.InsertRFIDEventParams{ID: id, CompanyID: rd.CompanyID, StoreID: rd.StoreID, ReaderID: rd.ID, ClientEventID: e.ClientEventID, Epc: e.epc,
				Direction: dir, Rssi: e.RSSI, ReadAt: e.ReadAt, LocalDecision: e.LocalDecision, Decision: dec.Result, Reason: dec.Reason, SecurityEventID: secID})
			if errors.Is(ierr, pgx.ErrNoRows) {
				res.Duplicate = true // already stored by an earlier attempt
			} else if ierr != nil {
				return ierr
			} else if err := q.TouchTagSeen(ctx, gen.TouchTagSeenParams{CompanyID: rd.CompanyID, Epc: e.epc, LastSeenAt: &e.ReadAt, LastReaderID: &rd.ID}); err != nil {
				return err
			}
			res.Decision, res.Reason, res.SecurityEventID = dec.Result, dec.Reason, secID
			results = append(results, res)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// raise records the security event for an alarmed read (deduplicated per reader/tag/minute) and notifies.
func (d *Device) raise(ctx context.Context, q *gen.Queries, rd gen.GetReaderByTokenRow, e EventIn, epc string, readAt time.Time, dec Decision, info *gen.GateLookupTagsRow) (*uuid.UUID, error) {
	var variant *uuid.UUID
	product := ""
	if info != nil {
		variant = &info.VariantID
		if t, err := q.GetTagByEPC(ctx, gen.GetTagByEPCParams{CompanyID: rd.CompanyID, Epc: epc}); err == nil {
			product = t.ProductName
			if t.VariantName != "" {
				product += " " + t.VariantName
			}
		}
	}
	title := map[string]string{
		security.UnauthorizedExit: "Barang keluar tanpa pembayaran",
		security.RFIDMismatch:     "Anomali tag RFID di gerbang",
		security.UnknownTag:       "Tag tidak dikenal di gerbang",
	}[dec.Type]
	if product != "" {
		title += ": " + product
	}
	detail := map[string]any{"reason": dec.Reason, "reader": rd.Code, "direction": e.Direction, "local_decision": e.LocalDecision, "local_missed": e.LocalDecision == "allow" && dec.Result == Alarm}
	if info != nil {
		detail["tag_status"] = info.Status
	}
	id, created, err := security.Record(ctx, q, security.Event{CompanyID: rd.CompanyID, StoreID: rd.StoreID, ReaderID: &rd.ID, Type: dec.Type, Severity: dec.Severity, Title: title, EPC: epc,
		VariantID: variant, Detail: detail, OccurredAt: readAt, Dedupe: fmt.Sprintf("gate:%s:%s:%d", rd.ID, epc, readAt.Unix()/60)})
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, nil
	}
	sev := notification.Warning
	if dec.Severity == "critical" {
		sev = notification.Critical
	}
	if dec.Severity == "info" {
		sev = notification.Info
	}
	sid := id
	if _, err := notification.Send(ctx, q, notification.Raise{CompanyID: rd.CompanyID, StoreID: &rd.StoreID, Perm: security.PermView, Type: "security_alert", Severity: sev,
		Title: title, Body: rd.StoreName + " · " + rd.Code + " · " + dec.Reason, EntityType: "security_event", EntityID: id.String(), Dedupe: "sec:" + id.String()}); err != nil {
		return nil, err
	}
	return &sid, nil
}

// ---- delta snapshot

type SnapshotTag struct {
	EPC       string     `json:"epc"`
	VariantID uuid.UUID  `json:"variant_id"`
	Status    string     `json:"status"`
	SoldAt    *time.Time `json:"sold_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

const snapshotPage = 5000

// snapshot returns tags of the reader's store changed since `since` (keyset paging on (updated_at, epc), so any
// number of tags sharing a timestamp pages correctly). Page with next_since/next_after while `more` is true; once
// caught up, continue from next_since minus a short overlap (the edge applies idempotently), which covers commits
// that land out of order.
func (d *Device) snapshot(w http.ResponseWriter, r *http.Request) {
	rd := readerFrom(r.Context())
	since := time.Time{}
	if s := r.URL.Query().Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			d.Fail(w, r, apperr.BadRequest("since must be RFC3339"))
			return
		}
		since = t
	}
	after := r.URL.Query().Get("after")
	limit := snapshotPage
	if n, _ := strconv.Atoi(r.URL.Query().Get("limit")); n > 0 && n < limit {
		limit = n
	}
	serverTime := d.now().UTC()
	q := d.tx.Queries()
	rules, err := d.rules(r.Context(), q, rd)
	if err != nil {
		d.Fail(w, r, err)
		return
	}
	rows, err := q.TagDelta(r.Context(), gen.TagDeltaParams{CompanyID: rd.CompanyID, StoreID: &rd.StoreID, Since: since, AfterEpc: after, RowLimit: int32(limit)})
	if err != nil {
		d.Fail(w, r, err)
		return
	}
	out := make([]SnapshotTag, len(rows))
	for i, t := range rows {
		out[i] = SnapshotTag{EPC: t.Epc, VariantID: t.VariantID, Status: t.Status, SoldAt: t.SoldAt, UpdatedAt: t.UpdatedAt}
	}
	next, nextAfter := serverTime, ""
	more := len(rows) == limit
	if more {
		next, nextAfter = rows[len(rows)-1].UpdatedAt, rows[len(rows)-1].Epc
	}
	response.OK(w, map[string]any{"server_time": serverTime, "tags": out, "more": more, "next_since": next, "next_after": nextAfter, "rules": rules})
}

// ---- heartbeat

type heartbeatRequest struct {
	AgentVersion string `json:"agent_version" validate:"max=40"`
	Buffered     int    `json:"buffered"`
	LocalAlarms  int    `json:"local_alarms"`
	SnapshotTags int    `json:"snapshot_tags"`
	SnapshotAge  int    `json:"snapshot_age_seconds"`
}

func (d *Device) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req heartbeatRequest
	if err := httpx.Bind(r, &req); err != nil {
		d.Fail(w, r, err)
		return
	}
	rd := readerFrom(r.Context())
	stats, _ := json.Marshal(map[string]any{"buffered": req.Buffered, "local_alarms": req.LocalAlarms, "snapshot_tags": req.SnapshotTags, "snapshot_age_seconds": req.SnapshotAge})
	q := d.tx.Queries()
	if err := q.TouchReader(r.Context(), gen.TouchReaderParams{ID: rd.ID, Ip: reqmeta.From(r.Context()).IP, Version: req.AgentVersion, Stats: stats}); err != nil {
		d.Fail(w, r, err)
		return
	}
	rules, err := d.rules(r.Context(), q, rd)
	if err != nil {
		d.Fail(w, r, err)
		return
	}
	response.OK(w, map[string]any{"server_time": d.now().UTC(), "rules": rules, "reader": map[string]any{"code": rd.Code, "kind": rd.Kind, "store": rd.StoreName}})
}

// ---- offline watchdog

// CheckReaders raises READER_OFFLINE for active gate readers that stopped reporting: a silent gate is the same as
// no gate, so loss prevention must hear about it.
func CheckReaders(ctx context.Context, t *tx.Manager, now time.Time) (int, error) {
	n := 0
	err := t.Do(ctx, func(q *gen.Queries) error {
		rows, err := q.StaleGateReaders(ctx, now.Add(-security.ReaderOnlineWindow))
		if err != nil {
			return err
		}
		for _, r := range rows {
			seen := int64(0)
			last := "never"
			if r.LastSeenAt != nil {
				seen = r.LastSeenAt.Unix()
				last = r.LastSeenAt.UTC().Format(time.RFC3339)
			}
			rid := r.ID
			id, created, err := security.Record(ctx, q, security.Event{CompanyID: r.CompanyID, StoreID: r.StoreID, ReaderID: &rid, Type: security.ReaderOffline, Severity: "critical",
				Title: "Gerbang RFID " + r.Code + " tidak melapor", Detail: map[string]any{"last_seen": last, "reader": r.Code}, Dedupe: fmt.Sprintf("offline:%s:%d", r.ID, seen), OccurredAt: now})
			if err != nil {
				return err
			}
			if !created {
				continue
			}
			n++
			sid := r.StoreID
			if _, err := notification.Send(ctx, q, notification.Raise{CompanyID: r.CompanyID, StoreID: &sid, Perm: security.PermView, Type: "security_alert", Severity: notification.Critical,
				Title: "Gerbang RFID " + r.Code + " tidak melapor", Body: r.StoreName + " · terakhir terlihat " + last, EntityType: "security_event", EntityID: id.String(), Dedupe: "sec:" + id.String()}); err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}
