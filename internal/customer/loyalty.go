// Package customer manages customers/members, membership levels and the loyalty points ledger.
package customer

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/settings"
	"github.com/ritel35k/backend/pkg/apperr"
)

const (
	KindEarn          = "earn"
	KindRedeem        = "redeem"
	KindAdjust        = "adjust"
	KindReverseEarn   = "reverse_earn"
	KindRestoreRedeem = "restore_redeem"
	KindExpire        = "expire"
)

// Rules are the loyalty settings resolved for one store.
type Rules struct {
	Enabled          bool
	EarnPerAmount    decimal.Decimal
	RedeemValue      decimal.Decimal
	MinRedeemPoints  int64
	MaxRedeemPercent decimal.Decimal
	ExpiryDays       int64
}

func RulesFrom(s settings.Snapshot) Rules {
	return Rules{
		Enabled: s.Bool("loyalty.enabled"), EarnPerAmount: s.Decimal("loyalty.earn_per_amount"), RedeemValue: s.Decimal("loyalty.redeem_value"),
		MinRedeemPoints: s.Decimal("loyalty.min_redeem_points").IntPart(), MaxRedeemPercent: s.Decimal("loyalty.max_redeem_percent"), ExpiryDays: s.Decimal("loyalty.expiry_days").IntPart(),
	}
}

// PointsFor: floor(amount / earn_per_amount) points, scaled by the level multiplier, floored again.
func (r Rules) PointsFor(amount, multiplier decimal.Decimal) int64 {
	if !r.Enabled || !r.EarnPerAmount.IsPositive() || !amount.IsPositive() {
		return 0
	}
	base := amount.Div(r.EarnPerAmount).Floor()
	return base.Mul(multiplier).Floor().IntPart()
}

// Value is the rupiah worth of points.
func (r Rules) Value(points int64) decimal.Decimal {
	return r.RedeemValue.Mul(decimal.NewFromInt(points))
}

// MaxRedeemable is how many points may pay `billTotal`, given the balance and the percent cap.
func (r Rules) MaxRedeemable(balance int64, billTotal decimal.Decimal) int64 {
	if !r.Enabled || !r.RedeemValue.IsPositive() || balance <= 0 {
		return 0
	}
	pct := decimal.Min(r.MaxRedeemPercent, decimal.NewFromInt(100))
	capPts := billTotal.Mul(pct).Div(decimal.NewFromInt(100)).Div(r.RedeemValue).Floor().IntPart()
	if capPts > balance {
		capPts = balance
	}
	if capPts < r.MinRedeemPoints {
		return 0
	}
	return capPts
}

// post writes one ledger row and moves the cached balance. q must be transaction-bound; callers lock the
// customer row first (GetCustomerForUpdate) so concurrent sales serialize on it.
func post(ctx context.Context, q *gen.Queries, companyID, customerID uuid.UUID, kind string, points int64, saleID *uuid.UUID, expires *time.Time, note string, by *uuid.UUID) (int64, error) {
	bal, err := q.ChangePoints(ctx, gen.ChangePointsParams{ID: customerID, PointsBalance: points})
	if err != nil {
		return 0, err
	}
	id, _ := uuid.NewV7()
	return bal, q.InsertLoyalty(ctx, gen.InsertLoyaltyParams{ID: id, CompanyID: companyID, CustomerID: customerID, Kind: kind, Points: points, BalanceAfter: bal, SaleID: saleID, ExpiresAt: expires, Note: note, CreatedBy: by})
}

func Earn(ctx context.Context, q *gen.Queries, companyID, customerID, saleID uuid.UUID, points int64, expiryDays int64, now time.Time, by *uuid.UUID) error {
	if points <= 0 {
		return nil
	}
	var exp *time.Time
	if expiryDays > 0 {
		t := now.AddDate(0, 0, int(expiryDays))
		exp = &t
	}
	_, err := post(ctx, q, companyID, customerID, KindEarn, points, &saleID, exp, "", by)
	return err
}

// Redeem spends points; it fails (never goes negative) if the balance is too low.
func Redeem(ctx context.Context, q *gen.Queries, companyID, customerID, saleID uuid.UUID, points int64, by *uuid.UUID) error {
	c, err := q.GetCustomerForUpdate(ctx, gen.GetCustomerForUpdateParams{ID: customerID, CompanyID: companyID})
	if err != nil {
		return err
	}
	if c.PointsBalance < points {
		return apperr.Conflict("INSUFFICIENT_POINTS", "Not enough loyalty points").WithDetails(map[string]int64{"balance": c.PointsBalance, "requested": points})
	}
	_, err = post(ctx, q, companyID, customerID, KindRedeem, -points, &saleID, nil, "", by)
	return err
}

// ReverseSale undoes a sale's loyalty effects (void): earned points are taken back (the balance may go
// negative if they were already spent — the ledger stays exact), redeemed points are returned.
func ReverseSale(ctx context.Context, q *gen.Queries, companyID, customerID, saleID uuid.UUID, by *uuid.UUID) error {
	if _, err := q.GetCustomerForUpdate(ctx, gen.GetCustomerForUpdateParams{ID: customerID, CompanyID: companyID}); err != nil {
		return err
	}
	rows, err := q.LoyaltyForSale(ctx, &saleID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		switch r.Kind {
		case KindEarn:
			if _, err := post(ctx, q, companyID, customerID, KindReverseEarn, -r.Points, &saleID, nil, "void", by); err != nil {
				return err
			}
		case KindRedeem:
			if _, err := post(ctx, q, companyID, customerID, KindRestoreRedeem, -r.Points, &saleID, nil, "void", by); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReverseEarnPartial takes back a share of the points earned on a sale (a partial return).
func ReverseEarnPartial(ctx context.Context, q *gen.Queries, companyID, customerID, saleID uuid.UUID, points int64, note string, by *uuid.UUID) error {
	if points <= 0 {
		return nil
	}
	if _, err := q.GetCustomerForUpdate(ctx, gen.GetCustomerForUpdateParams{ID: customerID, CompanyID: companyID}); err != nil {
		return err
	}
	_, err := post(ctx, q, companyID, customerID, KindReverseEarn, -points, &saleID, nil, note, by)
	return err
}

// AfterPurchase adds to lifetime spend and promotes the member when a higher level is reached.
// Levels only ever go up automatically; demotion is a manual decision.
func AfterPurchase(ctx context.Context, q *gen.Queries, companyID, customerID uuid.UUID, spend decimal.Decimal) error {
	c, err := q.AddCustomerSpend(ctx, gen.AddCustomerSpendParams{ID: customerID, LifetimeSpend: spend})
	if err != nil {
		return err
	}
	target, err := q.HighestEligibleLevel(ctx, gen.HighestEligibleLevelParams{CompanyID: companyID, MinSpend: c.LifetimeSpend})
	if err != nil {
		return nil // no level qualifies; keep the current one
	}
	cur, err := q.GetLevel(ctx, gen.GetLevelParams{ID: c.LevelID, CompanyID: companyID})
	if err != nil {
		return err
	}
	if target.Rank > cur.Rank {
		if err := q.SetCustomerLevel(ctx, gen.SetCustomerLevelParams{ID: customerID, LevelID: target.ID}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{CompanyID: companyID, Action: "MEMBER_LEVEL_UP", Entity: "customer", EntityID: customerID.String(), New: map[string]any{"from": cur.Code, "to": target.Code}})
	}
	return nil
}
