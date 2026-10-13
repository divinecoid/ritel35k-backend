package pos

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/ritel35k/backend/db/gen"
	customerpkg "github.com/ritel35k/backend/internal/customer"
	"github.com/ritel35k/backend/internal/promotion"
	"github.com/ritel35k/backend/pkg/posoffline"
)

const (
	snapshotMemberDays  = 90
	snapshotMemberLimit = 20000
)

// BuildSnapshot assembles the offline catalogue for one terminal's store. Everything is read straight from the
// same tables the online pricing path uses, so the two cannot drift by construction (and a differential test
// proves they do not).
func BuildSnapshot(ctx context.Context, q *gen.Queries, companyID, terminalID uuid.UUID, now time.Time) (*posoffline.Snapshot, error) {
	cs, err := loadTerminalStore(ctx, q, companyID, terminalID)
	if err != nil {
		return nil, err
	}
	var addr struct {
		Line1 string `json:"line1"`
	}
	if raw, err := q.GetStoreAddress(ctx, cs.store.ID); err == nil {
		_ = json.Unmarshal(raw, &addr)
	}
	s := &posoffline.Snapshot{GeneratedAt: now.UTC(), StoreID: cs.store.ID, StoreCode: cs.store.Code, StoreName: cs.store.Name, Address: addr.Line1,
		Timezone: cs.store.Timezone, TerminalID: cs.term.ID, TerminalCode: cs.term.Code}
	snap := cs.snap
	rules := customerpkg.RulesFrom(snap)
	s.Settings = posoffline.Settings{PriceIncludesTax: snap.Bool("tax.price_includes_tax"), PPNPercent: snap.Decimal("tax.ppn_percent"),
		MaxDiscountPercent: snap.Decimal("pos.max_discount_percent"), PaymentMethods: snap.Strings("payment.methods"), AllowNegativeStock: snap.Bool("pos.allow_negative_stock"),
		LoyaltyEnabled: rules.Enabled, EarnPerAmount: rules.EarnPerAmount, ReceiptHeader: snap.String("receipt.header"), ReceiptFooter: snap.String("receipt.footer")}

	items, err := q.SnapshotItems(ctx, companyID)
	if err != nil {
		return nil, err
	}
	s.Items = make([]posoffline.Item, 0, len(items))
	for _, it := range items {
		name := it.ProductName
		if it.VariantName != "" {
			name += " " + it.VariantName
		}
		s.Items = append(s.Items, posoffline.Item{VariantID: it.VariantID, SKU: it.Sku, Name: name, Barcodes: it.Barcodes, TaxCode: it.TaxCode, AllowsDecimal: it.AllowsDecimal,
			CategoryID: it.CategoryID, BrandID: it.BrandID, RFIDTracked: it.IsRfidTracked})
	}
	lists, err := q.SnapshotPriceLists(ctx, companyID)
	if err != nil {
		return nil, err
	}
	for _, l := range lists {
		s.PriceLists = append(s.PriceLists, posoffline.PriceList{ID: l.ID, IsDefault: l.IsDefault, LevelID: l.MembershipLevelID})
	}
	prices, err := q.SnapshotPrices(ctx, gen.SnapshotPricesParams{CompanyID: companyID, StoreOrgUnitID: cs.store.OrgUnitID, Since: now})
	if err != nil {
		return nil, err
	}
	s.Prices = make([]posoffline.Price, 0, len(prices))
	for _, p := range prices {
		s.Prices = append(s.Prices, posoffline.Price{VariantID: p.VariantID, ListID: p.PriceListID, MinQty: p.MinQty, Price: p.Price, Depth: int(p.Depth), SourceName: p.SourceName,
			From: p.EffectiveFrom.UTC(), To: utcPtr(p.EffectiveTo)})
	}
	promos, err := promotion.LoadFor(ctx, q, companyID, cs.store.OrgUnitID, now, true)
	if err != nil {
		return nil, err
	}
	s.Promotions = promos
	members, err := q.SnapshotMembers(ctx, gen.SnapshotMembersParams{CompanyID: companyID, StoreID: &cs.store.ID, Since: now.AddDate(0, 0, -snapshotMemberDays), RowLimit: snapshotMemberLimit})
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		s.Members = append(s.Members, posoffline.Member{ID: m.ID, MemberNo: m.MemberNo, Name: m.Name, Phone: m.Phone, LevelID: m.LevelID, LevelName: m.LevelName,
			EarnMultiplier: m.EarnMultiplier, PointsBalance: m.PointsBalance})
	}
	s.Version = s.ComputeVersion()
	return s, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
