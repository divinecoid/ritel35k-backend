package pos

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	customerpkg "github.com/ritel35k/backend/internal/customer"
	"github.com/ritel35k/backend/internal/inventory"
	"github.com/ritel35k/backend/internal/notification"
	"github.com/ritel35k/backend/internal/numbering"
	"github.com/ritel35k/backend/internal/platform/outbox"
	"github.com/ritel35k/backend/internal/rfid"
	"github.com/ritel35k/backend/internal/security"
	"github.com/ritel35k/backend/internal/shift"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/posoffline"
)

// maxClockSkew: an occurred_at further in the future than this is a broken edge clock and is replaced by server time.
const maxClockSkew = 10 * time.Minute

// exception is raised for review while a command is applied (never blocks the command).
type exception struct {
	typ, severity, title string
	detail               map[string]any
}

// ApplyCommands replays the edge's queued commands for ONE terminal, in order. Each command commits on its own:
//
//   - a sale that already happened is recorded as given — differences become offline exceptions for a human,
//     they never make the server refuse money that is already in the till;
//   - replaying a command is harmless (duplicate), so a lost response can be retried forever;
//   - a command that can never succeed (rejected) is reported so the edge dead-letters it, and sales among them are also
//     kept as a REJECTED_SALE exception with the full payload, so no takings disappear;
//   - a transient failure (database down) stops the batch: the results so far are returned and the rest is retried later.
func (s *Service) ApplyCommands(ctx context.Context, companyID, terminalID uuid.UUID, cmds []posoffline.Command) []posoffline.CommandResult {
	out := make([]posoffline.CommandResult, 0, len(cmds))
	for _, c := range cmds {
		var res posoffline.CommandResult
		var err error
		switch c.Type {
		case posoffline.CmdShiftOpen:
			res, err = s.applyShiftOpen(ctx, companyID, terminalID, c)
		case posoffline.CmdSale:
			res, err = s.applySale(ctx, companyID, terminalID, c)
		case posoffline.CmdShiftClose:
			res, err = s.applyShiftClose(ctx, companyID, terminalID, c)
		default:
			res = posoffline.CommandResult{Status: posoffline.StatusRejected, Code: "UNKNOWN_COMMAND", Message: "unknown command type " + c.Type}
		}
		res.ID, res.Type = c.ID, c.Type
		if err != nil {
			if code, msg, ok := permanent(err); ok {
				res.Status, res.Code, res.Message = posoffline.StatusRejected, code, msg
				if c.Type == posoffline.CmdSale {
					if rerr := s.recordRejectedSale(ctx, companyID, terminalID, c, code, msg); rerr != nil {
						return out // could not even store the evidence: retry the whole thing later
					}
				}
			} else {
				return out
			}
		}
		out = append(out, res)
	}
	return out
}

// permanent classifies errors that retrying can never fix.
func permanent(err error) (code, msg string, ok bool) {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Code, ae.Message, true
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23503", "23514", "23502", "22P02", "22003": // foreign key, check, not null, bad text, out of range
			return "INVALID_DATA", pe.Message, true
		}
	}
	return "", "", false
}

func clampTime(t, now time.Time) time.Time {
	if t.IsZero() || t.After(now.Add(maxClockSkew)) {
		return now
	}
	return t.UTC()
}

// ---------- shifts ----------

func (s *Service) applyShiftOpen(ctx context.Context, companyID, terminalID uuid.UUID, c posoffline.Command) (posoffline.CommandResult, error) {
	if c.ShiftOpen == nil {
		return posoffline.CommandResult{}, apperr.BadRequest("shift_open payload missing")
	}
	q := s.tx.Queries()
	if sh, err := q.GetShiftByClientID(ctx, gen.GetShiftByClientIDParams{TerminalID: terminalID, ClientShiftID: &c.ID}); err == nil {
		return posoffline.CommandResult{Status: posoffline.StatusDuplicate, ShiftID: &sh.ID}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return posoffline.CommandResult{}, err
	}
	if c.ShiftOpen.OpeningCash.IsNegative() {
		return posoffline.CommandResult{}, apperr.Validation(map[string]string{"opening_cash": "must not be negative"})
	}
	var shiftID uuid.UUID
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		cs, err := loadTerminalStoreMode(ctx, q, companyID, terminalID, false)
		if err != nil {
			return err
		}
		id, _ := uuid.NewV7()
		at := clampTime(c.OccurredAt, s.now())
		sh, err := q.CreateOfflineShift(ctx, gen.CreateOfflineShiftParams{ID: id, CompanyID: companyID, StoreID: cs.store.ID, TerminalID: terminalID,
			CashierID: c.ShiftOpen.CashierID, OpeningCash: c.ShiftOpen.OpeningCash, OpenedAt: at, ClientShiftID: &c.ID})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			if pe.ConstraintName == "cashier_shifts_one_open_terminal" || pe.ConstraintName == "cashier_shifts_one_open_cashier" {
				return apperr.Conflict("SHIFT_CONFLICT", "A shift is already open for this cashier or terminal; offline sales are attached to it")
			}
			return errDuplicateTxn
		}
		if err != nil {
			return err
		}
		shiftID = sh.ID
		uid := c.ShiftOpen.CashierID
		return audit.Record(ctx, q, audit.Entry{CompanyID: companyID, UserID: &uid, StoreID: &cs.store.ID, Action: "SHIFT_OPENED", Entity: "shift", EntityID: id.String(),
			New: map[string]any{"opening_cash": c.ShiftOpen.OpeningCash.String(), "terminal": cs.term.Code, "offline": true}})
	})
	if errors.Is(err, errDuplicateTxn) {
		sh, gerr := q.GetShiftByClientID(ctx, gen.GetShiftByClientIDParams{TerminalID: terminalID, ClientShiftID: &c.ID})
		if gerr != nil {
			return posoffline.CommandResult{}, gerr
		}
		return posoffline.CommandResult{Status: posoffline.StatusDuplicate, ShiftID: &sh.ID}, nil
	}
	if err != nil {
		return posoffline.CommandResult{}, err
	}
	return posoffline.CommandResult{Status: posoffline.StatusApplied, ShiftID: &shiftID}, nil
}

// findShift resolves the shift a command refers to; a sale with no usable reference falls back to the shift that was
// running on the terminal at that moment.
func findShift(ctx context.Context, q *gen.Queries, companyID, terminalID uuid.UUID, ref posoffline.ShiftRef, at time.Time, fallback bool) (*gen.CashierShift, error) {
	var sh gen.CashierShift
	var err error
	switch {
	case ref.ShiftID != nil:
		sh, err = q.GetShiftForUpdate(ctx, gen.GetShiftForUpdateParams{ID: *ref.ShiftID, CompanyID: companyID})
	case ref.ClientShiftID != nil:
		sh, err = q.GetShiftByClientID(ctx, gen.GetShiftByClientIDParams{TerminalID: terminalID, ClientShiftID: ref.ClientShiftID})
		if err == nil {
			sh, err = q.GetShiftForUpdate(ctx, gen.GetShiftForUpdateParams{ID: sh.ID, CompanyID: companyID})
		}
	default:
		err = pgx.ErrNoRows
	}
	if errors.Is(err, pgx.ErrNoRows) && fallback {
		sh, err = q.FindShiftAt(ctx, gen.FindShiftAtParams{TerminalID: terminalID, OpenedAt: at})
		if err == nil {
			sh, err = q.GetShiftForUpdate(ctx, gen.GetShiftForUpdateParams{ID: sh.ID, CompanyID: companyID})
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sh.TerminalID != terminalID {
		return nil, nil
	}
	return &sh, nil
}

func (s *Service) applyShiftClose(ctx context.Context, companyID, terminalID uuid.UUID, c posoffline.Command) (posoffline.CommandResult, error) {
	if c.ShiftClose == nil {
		return posoffline.CommandResult{}, apperr.BadRequest("shift_close payload missing")
	}
	if c.ShiftClose.CountedCash.IsNegative() {
		return posoffline.CommandResult{}, apperr.Validation(map[string]string{"counted_cash": "must not be negative"})
	}
	var res posoffline.CommandResult
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		sh, err := findShift(ctx, q, companyID, terminalID, c.ShiftClose.ShiftRef, time.Time{}, false)
		if err != nil {
			return err
		}
		if sh == nil {
			return apperr.New(404, "SHIFT_NOT_FOUND", "The shift to close is unknown on the server")
		}
		if sh.Status != "open" {
			res = posoffline.CommandResult{Status: posoffline.StatusDuplicate, ShiftID: &sh.ID, Message: "shift was already closed"}
			return nil
		}
		if _, err := shift.CloseCore(ctx, q, *sh, sh.CashierID, c.ShiftClose.CountedCash, c.ShiftClose.Note, clampTime(c.OccurredAt, s.now())); err != nil {
			return err
		}
		res = posoffline.CommandResult{Status: posoffline.StatusApplied, ShiftID: &sh.ID}
		return nil
	})
	return res, err
}

// ---------- sales ----------

func (s *Service) applySale(ctx context.Context, companyID, terminalID uuid.UUID, c posoffline.Command) (posoffline.CommandResult, error) {
	sc := c.Sale
	if sc == nil {
		return posoffline.CommandResult{}, apperr.BadRequest("sale payload missing")
	}
	rq := s.tx.Queries()
	if r, err := rq.GetSaleByClientTxnFull(ctx, gen.GetSaleByClientTxnFullParams{TerminalID: terminalID, ClientTxnID: c.ID}); err == nil {
		return posoffline.CommandResult{Status: posoffline.StatusDuplicate, SaleID: &r.ID, DocumentNumber: r.DocumentNumber}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return posoffline.CommandResult{}, err
	}
	paid, err := validateOfflineSale(sc)
	if err != nil {
		return posoffline.CommandResult{}, err
	}

	var res posoffline.CommandResult
	err = s.tx.Do(ctx, func(q *gen.Queries) error {
		r, err := s.bookOfflineSale(ctx, q, companyID, terminalID, c, paid)
		res = r
		return err
	})
	if errors.Is(err, errDuplicateTxn) { // a concurrent replay won
		r, gerr := rq.GetSaleByClientTxnFull(ctx, gen.GetSaleByClientTxnFullParams{TerminalID: terminalID, ClientTxnID: c.ID})
		if gerr != nil {
			return posoffline.CommandResult{}, gerr
		}
		return posoffline.CommandResult{Status: posoffline.StatusDuplicate, SaleID: &r.ID, DocumentNumber: r.DocumentNumber}, nil
	}
	return res, err
}

// validateOfflineSale checks the command is internally consistent (the numbers a till printed must add up) and
// returns the money actually applied to the bill.
func validateOfflineSale(sc *posoffline.SaleCommand) (decimal.Decimal, error) {
	if len(sc.Lines) == 0 {
		return decimal.Zero, apperr.New(422, "EMPTY_SALE", "A sale needs at least one line")
	}
	if sc.GrandTotal.IsNegative() || sc.CashierID == uuid.Nil {
		return decimal.Zero, apperr.New(422, "INVALID_SALE", "Invalid totals or cashier")
	}
	sum := decimal.Zero
	for _, l := range sc.Lines {
		if !l.Qty.IsPositive() || l.LineTotal.IsNegative() {
			return decimal.Zero, apperr.New(422, "INVALID_SALE", "Invalid line quantity or total")
		}
		sum = sum.Add(l.LineTotal)
	}
	if !sum.Equal(sc.GrandTotal) {
		return decimal.Zero, apperr.New(422, "INCONSISTENT_SALE", "Line totals do not add up to the grand total")
	}
	paid := decimal.Zero
	for _, p := range sc.Payments {
		if !p.Amount.IsPositive() {
			return decimal.Zero, apperr.New(422, "INVALID_SALE", "Payment amounts must be positive")
		}
		paid = paid.Add(p.Amount)
	}
	if !paid.Equal(sc.GrandTotal) {
		return decimal.Zero, apperr.New(422, "INCONSISTENT_SALE", "Payments do not cover the grand total")
	}
	return paid, nil
}

func (s *Service) bookOfflineSale(ctx context.Context, q *gen.Queries, companyID, terminalID uuid.UUID, c posoffline.Command, paid decimal.Decimal) (posoffline.CommandResult, error) {
	sc := c.Sale
	now := s.now()
	at := clampTime(c.OccurredAt, now)
	cs, err := loadTerminalStoreMode(ctx, q, companyID, terminalID, false)
	if err != nil {
		return posoffline.CommandResult{}, err
	}
	var exs []exception

	sh, err := findShift(ctx, q, companyID, terminalID, sc.ShiftRef, at, true)
	if err != nil {
		return posoffline.CommandResult{}, err
	}
	if sh == nil {
		return posoffline.CommandResult{}, apperr.New(409, "NO_SHIFT", "No shift on this terminal covers the sale")
	}
	if sh.Status != "open" {
		exs = append(exs, exception{posoffline.ExLateSale, "warning", "Offline sale synced into a closed shift",
			map[string]any{"shift_id": sh.ID, "shift_closed_at": sh.ClosedAt, "note": "the shift's expected cash changed after it was closed"}})
	}
	wh, err := q.GetDefaultWarehouse(ctx, &cs.store.ID)
	if err != nil {
		return posoffline.CommandResult{}, apperr.Conflict("NO_WAREHOUSE", "Store has no active default warehouse")
	}

	// the member may have been merged/deactivated since the snapshot: the sale stands, the points do not
	var member *gen.GetCustomerRow
	if sc.CustomerID != nil {
		m, err := q.GetCustomer(ctx, gen.GetCustomerParams{ID: *sc.CustomerID, CompanyID: companyID})
		switch {
		case err == nil && m.Status == "active":
			member = &m
		case err == nil || errors.Is(err, pgx.ErrNoRows):
			exs = append(exs, exception{posoffline.ExMemberMissing, "info", "Member not found or inactive; no points credited", map[string]any{"customer_id": sc.CustomerID}})
		default:
			return posoffline.CommandResult{}, err
		}
	}

	// what the server would have charged at that moment, for comparison only
	var server *Quote
	{
		items := make([]ItemInput, len(sc.Lines))
		for i, l := range sc.Lines {
			vid := l.VariantID
			items[i] = ItemInput{VariantID: &vid, Qty: l.Qty, DiscountPercent: l.DiscountPercent}
		}
		in := CartInput{TerminalID: terminalID, Items: items, CartDiscountPercent: sc.CartDiscountPct, skipTagRule: true}
		if member != nil {
			in.CustomerID = &member.ID
		}
		pr := &access.Principal{UserID: sc.CashierID, CompanyID: companyID, Grants: access.NewGrants()}
		if sq, _, perr := s.priceCartAt(ctx, q, pr, cs, in, at); perr == nil {
			server = sq
		} else if ae := (*apperr.Error)(nil); errors.As(perr, &ae) {
			exs = append(exs, exception{posoffline.ExUnpriceable, "warning", "The server could not re-price this offline sale", map[string]any{"code": ae.Code, "message": ae.Message}})
		} else {
			return posoffline.CommandResult{}, perr
		}
	}
	var critical *exception
	if server != nil {
		if ex := comparePrices(sc, server, cs.snap.Decimal("offline.price_alert_amount")); ex != nil {
			exs = append(exs, *ex)
			if ex.severity == "critical" {
				critical = &exs[len(exs)-1]
			}
		}
	}
	enabled := map[string]bool{}
	for _, m := range cs.snap.Strings("payment.methods") {
		enabled[m] = true
	}
	for _, p := range sc.Payments {
		if !enabled[p.Method] {
			exs = append(exs, exception{posoffline.ExPaymentMethod, "info", "Payment method is not enabled for the store: " + p.Method, map[string]any{"method": p.Method, "amount": p.Amount.String()}})
		}
	}

	// 1) stock — stable variant order against deadlocks; goods are gone, so never refuse on stock
	saleUUID, _ := uuid.NewV7()
	order := make([]int, len(sc.Lines))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return sc.Lines[order[a]].VariantID.String() < sc.Lines[order[b]].VariantID.String()
	})
	costs := make([]decimal.Decimal, len(sc.Lines))
	allowNeg := cs.snap.Bool("pos.allow_negative_stock")
	negative := map[uuid.UUID]string{}
	for _, i := range order {
		l := sc.Lines[i]
		r, err := inventory.Apply(ctx, q, inventory.Movement{CompanyID: companyID, StoreID: &cs.store.ID, WarehouseID: wh.ID, VariantID: l.VariantID, Type: inventory.Sale,
			Qty: l.Qty.Neg(), RefType: "sale", RefID: &saleUUID, By: &sc.CashierID, AllowNegative: true, At: &at})
		if err != nil {
			return posoffline.CommandResult{}, err
		}
		costs[i] = r.AvgCost
		if r.OnHand.IsNegative() && !allowNeg {
			negative[l.VariantID] = r.OnHand.String()
		}
	}
	for vid, bal := range negative {
		exs = append(exs, exception{posoffline.ExNegativeStock, "warning", "Offline sale took stock below zero", map[string]any{"variant_id": vid, "on_hand_after": bal}})
	}

	// 2) loyalty: points are earned at sync from the server's rules (the edge only previews them)
	var earned int64
	if member != nil {
		if _, err := q.GetCustomerForUpdate(ctx, gen.GetCustomerForUpdateParams{ID: member.ID, CompanyID: companyID}); err != nil {
			return posoffline.CommandResult{}, err
		}
		rules := customerpkg.RulesFrom(cs.snap)
		earned = rules.PointsFor(sc.GrandTotal, member.EarnMultiplier)
		if err := customerpkg.Earn(ctx, q, companyID, member.ID, saleUUID, earned, rules.ExpiryDays, at, &sc.CashierID); err != nil {
			return posoffline.CommandResult{}, err
		}
		if err := customerpkg.AfterPurchase(ctx, q, companyID, member.ID, sc.GrandTotal); err != nil {
			return posoffline.CommandResult{}, err
		}
	}

	// 3) the official number is assigned now; the offline receipt carried local_number
	doc, err := numbering.Next(ctx, q, companyID, cs.store.ID, "sale", at)
	if err != nil {
		return posoffline.CommandResult{}, err
	}
	loc, _ := time.LoadLocation(cs.store.Timezone)
	if loc == nil {
		loc = time.UTC
	}
	bizDate := at.In(loc)
	var custID *uuid.UUID
	if member != nil {
		custID = &member.ID
	}
	sale, err := q.InsertOfflineSale(ctx, gen.InsertOfflineSaleParams{ID: saleUUID, CompanyID: companyID, StoreID: cs.store.ID, TerminalID: terminalID, ShiftID: sh.ID,
		CashierID: sc.CashierID, DocumentNumber: doc, ClientTxnID: c.ID, BusinessDate: dateOnly(bizDate), Subtotal: sc.Subtotal, DiscountTotal: sc.DiscountTotal, TaxTotal: sc.TaxTotal,
		GrandTotal: sc.GrandTotal, PaidTotal: paid, ChangeAmount: sc.Change, CustomerID: custID, PromoDiscount: sc.PromoDiscount, PointsEarned: earned, OccurredAt: &at,
		LocalNumber: sc.LocalNumber, SnapshotVersion: sc.SnapshotVersion})
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "sales_terminal_id_client_txn_id_key" {
			return posoffline.CommandResult{}, errDuplicateTxn
		}
		return posoffline.CommandResult{}, err
	}

	// RFID: tags leave as SOLD when still sellable; a tag that is not (sold twice, moved) is a conflict to review, never a refusal
	var epcs []string
	for _, l := range sc.Lines {
		if l.EPC != "" {
			n, nerr := rfid.NormalizeEPC(l.EPC)
			if nerr != nil {
				n = l.EPC
			}
			epcs = append(epcs, n)
		}
	}
	if len(epcs) > 0 {
		n, err := q.SellTags(ctx, gen.SellTagsParams{CompanyID: companyID, Epcs: epcs, StoreID: &cs.store.ID, SaleID: &sale.ID})
		if err != nil {
			return posoffline.CommandResult{}, err
		}
		if int(n) != len(epcs) {
			exs = append(exs, exception{posoffline.ExTagConflict, "critical", "An RFID tag in an offline sale was not sellable when synced",
				map[string]any{"epcs": epcs, "sold_now": n, "expected": len(epcs)}})
		}
	}
	for i, l := range sc.Lines {
		iid, _ := uuid.NewV7()
		if _, err := q.InsertSaleItem(ctx, gen.InsertSaleItemParams{ID: iid, SaleID: sale.ID, LineNo: int32(i + 1), VariantID: l.VariantID, Sku: l.SKU, ProductName: l.Name,
			Barcode: l.Barcode, Qty: l.Qty, UnitPrice: l.UnitPrice, DiscountPercent: l.DiscountPercent, DiscountAmount: l.DiscountAmount, TaxRate: l.TaxRate, TaxAmount: l.TaxAmount,
			LineTotal: l.LineTotal, UnitCost: costs[i], WarehouseID: wh.ID, PromoDiscount: l.PromoDiscount}); err != nil {
			return posoffline.CommandResult{}, err
		}
		if l.EPC != "" {
			epc := l.EPC
			if n, nerr := rfid.NormalizeEPC(epc); nerr == nil {
				epc = n
			}
			if err := q.InsertSaleItemTag(ctx, gen.InsertSaleItemTagParams{SaleItemID: iid, Epc: epc}); err != nil {
				return posoffline.CommandResult{}, err
			}
		}
		for _, a := range l.Promotions {
			if err := q.InsertSaleItemPromotion(ctx, gen.InsertSaleItemPromotionParams{SaleItemID: iid, PromotionID: a.PromotionID, Name: a.Name, Amount: a.Amount}); err != nil {
				return posoffline.CommandResult{}, err
			}
		}
	}
	for _, p := range sc.Payments {
		pid, _ := uuid.NewV7()
		tendered := p.Tendered
		if !tendered.IsPositive() {
			tendered = p.Amount
		}
		if _, err := q.InsertPayment(ctx, gen.InsertPaymentParams{ID: pid, SaleID: sale.ID, Method: p.Method, Amount: p.Amount, Tendered: tendered, Reference: p.Reference}); err != nil {
			return posoffline.CommandResult{}, err
		}
	}

	var types []string
	for _, e := range exs {
		eid, _ := uuid.NewV7()
		detail, _ := json.Marshal(e.detail)
		if _, err := q.InsertOfflineException(ctx, gen.InsertOfflineExceptionParams{ID: eid, CompanyID: companyID, StoreID: cs.store.ID, TerminalID: &terminalID, SaleID: &sale.ID,
			ShiftID: &sh.ID, Type: e.typ, Severity: e.severity, Title: e.title, Detail: detail, DedupeKey: "sale:" + c.ID.String() + ":" + e.typ}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return posoffline.CommandResult{}, err
		}
		types = append(types, e.typ)
	}
	if critical != nil {
		if err := s.alertCritical(ctx, q, companyID, cs, sale, sc, critical); err != nil {
			return posoffline.CommandResult{}, err
		}
	}
	if err := outbox.Publish(ctx, q, companyID, EventSaleCompleted, "sale", sale.ID, map[string]any{
		"store_id": cs.store.ID, "terminal_id": terminalID, "shift_id": sh.ID, "document_number": doc, "grand_total": sc.GrandTotal.String(),
		"discount_total": sc.DiscountTotal.String(), "promo_discount": sc.PromoDiscount.String(), "voucher_discount": "0",
		"tax_total": sc.TaxTotal.String(), "items": len(sc.Lines), "business_date": bizDate.Format("2006-01-02"), "customer_id": custID,
		"points_earned": earned, "points_redeemed": 0, "offline": true, "occurred_at": at}); err != nil {
		return posoffline.CommandResult{}, err
	}
	return posoffline.CommandResult{Status: posoffline.StatusApplied, SaleID: &sale.ID, ShiftID: &sh.ID, DocumentNumber: doc, Exceptions: types}, nil
}

// comparePrices flags any difference between what the till charged and what the server computes for that moment.
func comparePrices(sc *posoffline.SaleCommand, server *Quote, alert decimal.Decimal) *exception {
	diff := sc.GrandTotal.Sub(server.GrandTotal)
	var lines []map[string]any
	for i, l := range sc.Lines {
		if i >= len(server.Lines) {
			break
		}
		if sl := server.Lines[i]; !l.UnitPrice.Equal(sl.UnitPrice) {
			lines = append(lines, map[string]any{"sku": l.SKU, "offline_price": l.UnitPrice.String(), "server_price": sl.UnitPrice.String()})
		}
	}
	if diff.IsZero() && len(lines) == 0 {
		return nil
	}
	sev := "warning"
	if alert.IsPositive() && diff.Abs().GreaterThanOrEqual(alert) {
		sev = "critical"
	}
	return &exception{posoffline.ExPriceMismatch, sev, "Offline sale total differs from the server's price",
		map[string]any{"offline_total": sc.GrandTotal.String(), "server_total": server.GrandTotal.String(), "difference": diff.String(), "lines": lines, "snapshot_version": sc.SnapshotVersion}}
}

func (s *Service) alertCritical(ctx context.Context, q *gen.Queries, companyID uuid.UUID, cs *ctxStore, sale gen.Sale, sc *posoffline.SaleCommand, ex *exception) error {
	if _, _, err := security.Record(ctx, q, security.Event{CompanyID: companyID, StoreID: cs.store.ID, Type: security.OfflinePrice, Severity: "critical",
		Title: "Offline sale priced far from the server's price", SaleID: &sale.ID, UserID: &sc.CashierID, Detail: ex.detail, Dedupe: "offline-price:" + sale.ID.String(), OccurredAt: sale.CreatedAt}); err != nil {
		return err
	}
	_, err := notification.Send(ctx, q, notification.Raise{CompanyID: companyID, StoreID: &cs.store.ID, Perm: security.PermView, Type: "OFFLINE_PRICE_MISMATCH", Severity: notification.Critical,
		Title: "Offline sale with a large price difference", Body: cs.store.Name + " · " + sale.DocumentNumber + " · " + notification.IDR(sc.GrandTotal),
		EntityType: "sale", EntityID: sale.ID.String(), Dedupe: "offline-price:" + sale.ID.String()})
	return err
}

// recordRejectedSale keeps the full payload of a sale that could not be booked, so the takings can be entered by hand.
func (s *Service) recordRejectedSale(ctx context.Context, companyID, terminalID uuid.UUID, c posoffline.Command, code, msg string) error {
	q := s.tx.Queries()
	term, err := q.GetTerminal(ctx, gen.GetTerminalParams{ID: terminalID, CompanyID: companyID})
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"code": code, "message": msg, "command": c})
	var grand decimal.Decimal
	if c.Sale != nil {
		grand = c.Sale.GrandTotal
	}
	return s.tx.Do(ctx, func(q *gen.Queries) error {
		eid, _ := uuid.NewV7()
		if _, err := q.InsertOfflineException(ctx, gen.InsertOfflineExceptionParams{ID: eid, CompanyID: companyID, StoreID: term.StoreID, TerminalID: &terminalID,
			Type: posoffline.ExRejectedSale, Severity: "critical", Title: "Offline sale could not be booked (" + code + ")", Detail: payload, DedupeKey: "rejected:" + c.ID.String()}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err := notification.Send(ctx, q, notification.Raise{CompanyID: companyID, StoreID: &term.StoreID, Perm: security.PermView, Type: "OFFLINE_SALE_REJECTED", Severity: notification.Critical,
			Title: "An offline sale could not be booked", Body: term.Code + " · " + notification.IDR(grand) + " · " + code, EntityType: "offline_exception", EntityID: c.ID.String(), Dedupe: "rejected:" + c.ID.String()})
		return err
	})
}
