package stockcount

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/rfid"
	"github.com/ritel35k/backend/pkg/apperr"
)

// RFIDScanResult tells the counter what happened to every tag of a scan batch.
type RFIDScanResult struct {
	Added        int               `json:"added"`           // new tags counted
	AlreadyCount int               `json:"already_counted"` // seen in an earlier batch of this count
	Unknown      []string          `json:"unknown"`         // not in the tag register
	Foreign      []string          `json:"foreign"`         // registered to another store
	Sold         []string          `json:"sold"`            // physically here although the system says SOLD
	Flagged      []string          `json:"flagged"`         // marked LOST/DAMAGED/REGISTERED but found in the store
	NotInCount   []string          `json:"not_in_count"`    // product is outside this (partial/category) count
	Totals       map[string]string `json:"totals"`          // variant_id → counted quantity now
}

// ScanRFID counts a batch of tags. The counted quantity of every RFID-tracked variant becomes the number of distinct
// valid tags seen so far in this count, so batches can overlap and be resent. For tracked products RFID is the
// authority and replaces any barcode count of the same variant.
func (s *Service) ScanRFID(ctx context.Context, id uuid.UUID, epcsIn []string) (*RFIDScanResult, error) {
	p := access.MustPrincipal(ctx)
	if len(epcsIn) == 0 || len(epcsIn) > 5000 {
		return nil, apperr.Validation(map[string]string{"epcs": "1-5000 tags per batch"})
	}
	out := &RFIDScanResult{Unknown: []string{}, Foreign: []string{}, Sold: []string{}, Flagged: []string{}, NotInCount: []string{}, Totals: map[string]string{}}
	seen := map[string]bool{}
	var epcs []string
	for _, e := range epcsIn {
		n, err := rfid.NormalizeEPC(e)
		if err != nil {
			return nil, apperr.Validation(map[string]string{"epcs": "invalid EPC: " + e})
		}
		if !seen[n] {
			seen[n] = true
			epcs = append(epcs, n)
		}
	}
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		c, _, err := s.open(ctx, q, p, id)
		if err != nil {
			return err
		}
		if c.StoreID == nil {
			return apperr.Conflict("COUNT_NOT_IN_STORE", "RFID counting works for store counts; this count is for a central warehouse")
		}
		known, err := q.GateLookupTags(ctx, gen.GateLookupTagsParams{CompanyID: p.CompanyID, Epcs: epcs})
		if err != nil {
			return err
		}
		byEPC := map[string]gen.GateLookupTagsRow{}
		for _, t := range known {
			byEPC[t.Epc] = t
		}
		items, err := q.ListCountItems(ctx, id)
		if err != nil {
			return err
		}
		inCount := map[uuid.UUID]bool{}
		for _, i := range items {
			inCount[i.VariantID] = true
		}
		var countable []string
		affected := map[uuid.UUID]bool{}
		for _, e := range epcs {
			t, ok := byEPC[e]
			switch {
			case !ok:
				out.Unknown = append(out.Unknown, e)
			case t.StoreID == nil || *t.StoreID != *c.StoreID:
				out.Foreign = append(out.Foreign, e)
			case t.Status == rfid.Sold:
				out.Sold = append(out.Sold, e)
			case !rfid.Present(t.Status):
				out.Flagged = append(out.Flagged, e)
			case !inCount[t.VariantID] && c.Kind != "full":
				out.NotInCount = append(out.NotInCount, e)
			default:
				countable = append(countable, e)
				affected[t.VariantID] = true
			}
		}
		if len(countable) == 0 {
			return nil
		}
		// a full count accepts a tracked variant the system thinks is not here (surplus: system qty 0)
		for v := range affected {
			if inCount[v] {
				continue
			}
			vr, err := q.GetVariant(ctx, gen.GetVariantParams{ID: v, CompanyID: p.CompanyID})
			if err != nil {
				return err
			}
			iid, _ := uuid.NewV7()
			if err := q.InsertCountItemIfMissing(ctx, gen.InsertCountItemIfMissingParams{ID: iid, CountID: id, VariantID: v, SystemQty: decimal.Zero, UnitCost: vr.Cost}); err != nil {
				return err
			}
		}
		added, err := q.AddCountTags(ctx, gen.AddCountTagsParams{CountID: id, CompanyID: p.CompanyID, Epcs: countable, StoreID: c.StoreID})
		if err != nil {
			return err
		}
		out.Added = int(added)
		out.AlreadyCount = len(countable) - int(added)
		vars := make([]uuid.UUID, 0, len(affected))
		for v := range affected {
			vars = append(vars, v)
		}
		totals, err := q.CountTagTotals(ctx, gen.CountTagTotalsParams{CountID: id, Variants: vars})
		if err != nil {
			return err
		}
		for _, t := range totals {
			qty := decimal.NewFromInt(t.N)
			if _, err := q.SetCountedQty(ctx, gen.SetCountedQtyParams{CountID: id, VariantID: t.VariantID, CountedQty: &qty}); err != nil {
				return err
			}
			out.Totals[t.VariantID.String()] = qty.String()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type rfidScanRequest struct {
	EPCs []string `json:"epcs" validate:"required,min=1,max=5000"`
}

func (h *Handler) scanRFID(w http.ResponseWriter, r *http.Request) {
	var req rfidScanRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	h.id(func(r *http.Request, id uuid.UUID) (any, error) { return h.svc.ScanRFID(r.Context(), id, req.EPCs) })(w, r)
}
