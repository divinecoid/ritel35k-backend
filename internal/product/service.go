// Package product manages the global product master: products, SKU variants and barcodes.
package product

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/internal/pricing"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

type Service struct{ tx *tx.Manager }

func NewService(t *tx.Manager) *Service { return &Service{tx: t} }

type Barcode struct {
	ID        uuid.UUID `json:"id"`
	Barcode   string    `json:"barcode"`
	Kind      string    `json:"kind"`
	IsPrimary bool      `json:"is_primary"`
}

type Variant struct {
	ID            uuid.UUID       `json:"id"`
	SKU           string          `json:"sku"`
	Name          string          `json:"name"`
	Attributes    json.RawMessage `json:"attributes"`
	PurchasePrice decimal.Decimal `json:"purchase_price"`
	Cost          decimal.Decimal `json:"cost"`
	MinStock      decimal.Decimal `json:"min_stock"`
	MaxStock      decimal.Decimal `json:"max_stock"`
	ReorderPoint  decimal.Decimal `json:"reorder_point"`
	Status        string          `json:"status"`
	Barcodes      []Barcode       `json:"barcodes"`
}

type Product struct {
	ID            uuid.UUID  `json:"id"`
	Code          string     `json:"code"`
	Name          string     `json:"name"`
	ShortName     string     `json:"short_name"`
	CategoryID    *uuid.UUID `json:"category_id"`
	BrandID       *uuid.UUID `json:"brand_id"`
	UOMID         uuid.UUID  `json:"uom_id"`
	TaxCode       string     `json:"tax_code"`
	ImageKey      string     `json:"image_key"`
	IsRFIDTracked bool       `json:"is_rfid_tracked"`
	Status        string     `json:"status"`
	Version       int32      `json:"version"`
	CreatedAt     time.Time  `json:"created_at"`
	Variants      []Variant  `json:"variants"`
}

func toProduct(r gen.Product) Product {
	return Product{ID: r.ID, Code: r.Code, Name: r.Name, ShortName: r.ShortName, CategoryID: r.CategoryID, BrandID: r.BrandID, UOMID: r.UomID,
		TaxCode: r.TaxCode, ImageKey: r.ImageKey, IsRFIDTracked: r.IsRfidTracked, Status: r.Status, Version: r.Version, CreatedAt: r.CreatedAt, Variants: []Variant{}}
}

func toVariant(v gen.ProductVariant) Variant {
	return Variant{ID: v.ID, SKU: v.Sku, Name: v.Name, Attributes: v.Attributes, PurchasePrice: v.PurchasePrice, Cost: v.Cost, MinStock: v.MinStock,
		MaxStock: v.MaxStock, ReorderPoint: v.ReorderPoint, Status: v.Status, Barcodes: []Barcode{}}
}

func hydrate(ctx context.Context, q *gen.Queries, rows []gen.Product) ([]Product, error) {
	out := make([]Product, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	pidx := map[uuid.UUID]int{}
	pids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = toProduct(r)
		pids[i] = r.ID
		pidx[r.ID] = i
	}
	vs, err := q.ListVariantsByProducts(ctx, pids)
	if err != nil {
		return nil, err
	}
	vids := make([]uuid.UUID, len(vs))
	type loc struct{ p, v int }
	vloc := map[uuid.UUID]loc{}
	for i, v := range vs {
		vids[i] = v.ID
		pi := pidx[v.ProductID]
		out[pi].Variants = append(out[pi].Variants, toVariant(v))
		vloc[v.ID] = loc{pi, len(out[pi].Variants) - 1}
	}
	bs, err := q.ListBarcodesByVariants(ctx, vids)
	if err != nil {
		return nil, err
	}
	for _, b := range bs {
		l := vloc[b.VariantID]
		out[l.p].Variants[l.v].Barcodes = append(out[l.p].Variants[l.v].Barcodes, Barcode{ID: b.ID, Barcode: b.Barcode, Kind: b.Kind, IsPrimary: b.IsPrimary})
	}
	return out, nil
}

type ListFilter struct {
	Q          string
	CategoryID *uuid.UUID
	BrandID    *uuid.UUID
	Status     string
}

func (s *Service) List(ctx context.Context, f ListFilter, pg pagination.Params) ([]Product, pagination.Meta, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	rows, err := q.ListProducts(ctx, gen.ListProductsParams{CompanyID: p.CompanyID, Q: f.Q, CategoryID: f.CategoryID, BrandID: f.BrandID, Status: f.Status,
		PageLimit: pg.Limit(), PageOffset: pg.Offset()})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	total, err := q.CountProducts(ctx, gen.CountProductsParams{CompanyID: p.CompanyID, Q: f.Q, CategoryID: f.CategoryID, BrandID: f.BrandID, Status: f.Status})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	out, err := hydrate(ctx, q, rows)
	return out, pagination.NewMeta(pg, total), err
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Product, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	r, err := q.GetProduct(ctx, gen.GetProductParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("product")
	}
	if err != nil {
		return nil, err
	}
	out, err := hydrate(ctx, q, []gen.Product{r})
	if err != nil {
		return nil, err
	}
	return &out[0], nil
}

// Lookup resolves a scanned barcode to exactly one variant (barcodes are unique per company).
func (s *Service) Lookup(ctx context.Context, barcode string) (*gen.LookupBarcodeRow, error) {
	p := access.MustPrincipal(ctx)
	r, err := s.tx.Queries().LookupBarcode(ctx, gen.LookupBarcodeParams{CompanyID: p.CompanyID, Barcode: barcode})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("barcode")
	}
	return &r, err
}

type BarcodeInput struct {
	Barcode   string
	Kind      string
	IsPrimary bool
}

type VariantInput struct {
	SKU           string
	Name          string
	Attributes    json.RawMessage
	PurchasePrice decimal.Decimal
	Cost          decimal.Decimal
	MinStock      decimal.Decimal
	MaxStock      decimal.Decimal
	ReorderPoint  decimal.Decimal
	// SellingPrice, if set, becomes the company-wide default price (stores/regions can override later).
	SellingPrice *decimal.Decimal
	Barcodes     []BarcodeInput
}

type CreateInput struct {
	Code          string
	Name          string
	ShortName     string
	CategoryID    *uuid.UUID
	BrandID       *uuid.UUID
	UOMID         uuid.UUID
	TaxCode       string
	IsRFIDTracked bool
	Variants      []VariantInput
}

func conflict(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		switch pe.ConstraintName {
		case "products_company_id_code_key":
			return apperr.Conflict("PRODUCT_CODE_TAKEN", "Product code already exists")
		case "product_variants_company_id_sku_key":
			return apperr.Conflict("SKU_TAKEN", "SKU already exists")
		case "barcodes_company_id_barcode_key":
			return apperr.Conflict("BARCODE_TAKEN", "Barcode already assigned to another item")
		}
		return apperr.Conflict("DUPLICATE", "Duplicate value")
	}
	if errors.As(err, &pe) && pe.Code == "23503" {
		return apperr.Validation(map[string]string{"reference": "unknown category, brand or uom"})
	}
	return err
}

func validateBarcode(b BarcodeInput) error {
	if b.Kind == "" || b.Kind == "EAN13" {
		if !ValidEAN13(b.Barcode) {
			return apperr.Validation(map[string]string{"barcode": "invalid EAN-13 (" + b.Barcode + "); use kind INTERNAL for non-EAN codes"})
		}
		return nil
	}
	if len(b.Barcode) < 1 || len(b.Barcode) > 64 {
		return apperr.Validation(map[string]string{"barcode": "1-64 chars"})
	}
	return nil
}

// ValidEAN13 verifies length, digits and the check digit.
func ValidEAN13(s string) bool {
	if len(s) != 13 {
		return false
	}
	sum := 0
	for i := 0; i < 12; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if i%2 == 1 {
			d *= 3
		}
		sum += d
	}
	last := s[12]
	return last >= '0' && last <= '9' && (10-sum%10)%10 == int(last-'0')
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*Product, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.ProductManage); err != nil {
		return nil, err
	}
	if len(in.Variants) == 0 {
		return nil, apperr.Validation(map[string]string{"variants": "at least one variant (SKU) is required"})
	}
	if in.TaxCode == "" {
		in.TaxCode = "PPN"
	}
	for _, v := range in.Variants {
		for _, b := range v.Barcodes {
			if err := validateBarcode(b); err != nil {
				return nil, err
			}
		}
	}
	var created gen.Product
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		pid, _ := uuid.NewV7()
		var err error
		created, err = q.CreateProduct(ctx, gen.CreateProductParams{ID: pid, CompanyID: p.CompanyID, Code: in.Code, Name: in.Name, ShortName: in.ShortName,
			CategoryID: in.CategoryID, BrandID: in.BrandID, UomID: in.UOMID, TaxCode: in.TaxCode, ImageKey: "", IsRfidTracked: in.IsRFIDTracked, CreatedBy: &p.UserID})
		if err != nil {
			return conflict(err)
		}
		for _, v := range in.Variants {
			if err := s.addVariant(ctx, q, p, pid, v); err != nil {
				return err
			}
		}
		return audit.Record(ctx, q, audit.Entry{Action: audit.ProductCreated, Entity: "product", EntityID: pid.String(), New: in})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, created.ID)
}

func (s *Service) addVariant(ctx context.Context, q *gen.Queries, p *access.Principal, productID uuid.UUID, v VariantInput) error {
	vid, _ := uuid.NewV7()
	attrs := v.Attributes
	if len(attrs) == 0 {
		attrs = json.RawMessage(`{}`)
	}
	if _, err := q.CreateVariant(ctx, gen.CreateVariantParams{ID: vid, CompanyID: p.CompanyID, ProductID: productID, Sku: v.SKU, Name: v.Name, Attributes: attrs,
		PurchasePrice: v.PurchasePrice, Cost: v.Cost, MinStock: v.MinStock, MaxStock: v.MaxStock, ReorderPoint: v.ReorderPoint, CreatedBy: &p.UserID}); err != nil {
		return conflict(err)
	}
	if v.SellingPrice != nil {
		root, err := q.GetCompanyRootOrgUnit(ctx, p.CompanyID)
		if err != nil {
			return err
		}
		if err := pricing.SetAt(ctx, q, p.CompanyID, root.ID, vid, nil, decimal.NewFromInt(1), *v.SellingPrice, time.Now(), &p.UserID); err != nil {
			return err
		}
	}
	for i, b := range v.Barcodes {
		bid, _ := uuid.NewV7()
		kind := b.Kind
		if kind == "" {
			kind = "EAN13"
		}
		if _, err := q.CreateBarcode(ctx, gen.CreateBarcodeParams{ID: bid, CompanyID: p.CompanyID, VariantID: vid, Barcode: b.Barcode, Kind: kind, IsPrimary: b.IsPrimary || i == 0}); err != nil {
			return conflict(err)
		}
	}
	return nil
}

type UpdateInput struct {
	Version       int32
	Name          *string
	ShortName     *string
	CategoryID    *uuid.UUID
	BrandID       *uuid.UUID
	UOMID         *uuid.UUID
	TaxCode       *string
	IsRFIDTracked *bool
	Status        *string
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (*Product, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.ProductManage); err != nil {
		return nil, err
	}
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		_, err := q.UpdateProduct(ctx, gen.UpdateProductParams{ID: id, CompanyID: p.CompanyID, Version: in.Version, Name: in.Name, ShortName: in.ShortName,
			CategoryID: in.CategoryID, BrandID: in.BrandID, UomID: in.UOMID, TaxCode: in.TaxCode, IsRfidTracked: in.IsRFIDTracked, Status: in.Status})
		if errors.Is(err, pgx.ErrNoRows) {
			if _, e := q.GetProduct(ctx, gen.GetProductParams{ID: id, CompanyID: p.CompanyID}); e == nil {
				return apperr.Conflict("VERSION_CONFLICT", "Product was modified by someone else; reload and retry")
			}
			return apperr.NotFound("product")
		}
		if err != nil {
			return conflict(err)
		}
		return audit.Record(ctx, q, audit.Entry{Action: audit.ProductUpdated, Entity: "product", EntityID: id.String(), New: in})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *Service) AddVariant(ctx context.Context, productID uuid.UUID, v VariantInput) (*Product, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.ProductManage); err != nil {
		return nil, err
	}
	for _, b := range v.Barcodes {
		if err := validateBarcode(b); err != nil {
			return nil, err
		}
	}
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		if _, err := q.GetProduct(ctx, gen.GetProductParams{ID: productID, CompanyID: p.CompanyID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.NotFound("product")
			}
			return err
		}
		if err := s.addVariant(ctx, q, p, productID, v); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: audit.ProductUpdated, Entity: "product", EntityID: productID.String(), New: v})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, productID)
}

func (s *Service) AddBarcode(ctx context.Context, variantID uuid.UUID, b BarcodeInput) (*Barcode, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.ProductManage); err != nil {
		return nil, err
	}
	if err := validateBarcode(b); err != nil {
		return nil, err
	}
	kind := b.Kind
	if kind == "" {
		kind = "EAN13"
	}
	var out Barcode
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		if _, err := q.GetVariant(ctx, gen.GetVariantParams{ID: variantID, CompanyID: p.CompanyID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.NotFound("variant")
			}
			return err
		}
		id, _ := uuid.NewV7()
		r, err := q.CreateBarcode(ctx, gen.CreateBarcodeParams{ID: id, CompanyID: p.CompanyID, VariantID: variantID, Barcode: b.Barcode, Kind: kind, IsPrimary: b.IsPrimary})
		if err != nil {
			return conflict(err)
		}
		out = Barcode{ID: r.ID, Barcode: r.Barcode, Kind: r.Kind, IsPrimary: r.IsPrimary}
		return audit.Record(ctx, q, audit.Entry{Action: audit.ProductUpdated, Entity: "barcode", EntityID: id.String(), New: out})
	})
	return &out, err
}

func (s *Service) DeleteBarcode(ctx context.Context, id uuid.UUID) error {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.ProductManage); err != nil {
		return err
	}
	return s.tx.Do(ctx, func(q *gen.Queries) error {
		n, err := q.DeleteBarcode(ctx, gen.DeleteBarcodeParams{ID: id, CompanyID: p.CompanyID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.NotFound("barcode")
		}
		return audit.Record(ctx, q, audit.Entry{Action: audit.ProductUpdated, Entity: "barcode", EntityID: id.String(), Old: "deleted"})
	})
}
