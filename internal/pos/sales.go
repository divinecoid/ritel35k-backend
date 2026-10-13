package pos

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

// detail loads a sale with items, payments and returns; the caller must be allowed to see it.
func (s *Service) detail(ctx context.Context, q *gen.Queries, p *access.Principal, id uuid.UUID) (*SaleDetail, error) {
	row, err := q.GetSale(ctx, gen.GetSaleParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("sale")
	}
	if err != nil {
		return nil, err
	}
	// the cashier who made the sale may always see it; others need pos.sale.view in the store's scope
	if row.CashierID != p.UserID && !p.Grants.Can(PermView, row.Path) {
		return nil, apperr.NotFound("sale")
	}
	items, err := q.ListSaleItems(ctx, id)
	if err != nil {
		return nil, err
	}
	pays, err := q.ListPayments(ctx, id)
	if err != nil {
		return nil, err
	}
	rets, err := q.ListReturnsBySale(ctx, id)
	if err != nil {
		return nil, err
	}
	promos, err := q.ListSaleItemPromotions(ctx, id)
	if err != nil {
		return nil, err
	}
	return &SaleDetail{GetSaleRow: row, Items: items, Payments: pays, Returns: rets, Promotions: promos}, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*SaleDetail, error) {
	return s.detail(ctx, s.tx.Queries(), access.MustPrincipal(ctx), id)
}

type SaleFilter struct {
	StoreID   *uuid.UUID
	ShiftID   *uuid.UUID
	UnderPath string
	Status    string
	Q         string
	From, To  *time.Time
}

func (s *Service) List(ctx context.Context, f SaleFilter, pg pagination.Params) ([]gen.ListSalesRow, pagination.Meta, error) {
	p := access.MustPrincipal(ctx)
	all, paths := p.Grants.Scope(PermView).SQL()
	q := s.tx.Queries()
	rows, err := q.ListSales(ctx, gen.ListSalesParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, UnderPath: f.UnderPath,
		Status: f.Status, Q: f.Q, ShiftID: f.ShiftID, DateFrom: f.From, DateTo: f.To, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	total, err := q.CountSales(ctx, gen.CountSalesParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, UnderPath: f.UnderPath,
		Status: f.Status, Q: f.Q, ShiftID: f.ShiftID, DateFrom: f.From, DateTo: f.To})
	return rows, pagination.NewMeta(pg, total), err
}
