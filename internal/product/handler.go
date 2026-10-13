package product

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	view, manage := g.Perm(access.ProductView), g.Perm(access.ProductManage)
	r.Route("/products", func(r chi.Router) {
		r.With(view).Get("/", h.list)
		r.With(view).Get("/lookup", h.lookup)
		r.With(view).Get("/{id}", h.get)
		r.With(manage).Post("/", h.create)
		r.With(manage).Patch("/{id}", h.update)
		r.With(manage).Post("/{id}/variants", h.addVariant)
	})
	r.With(manage).Post("/variants/{id}/barcodes", h.addBarcode)
	r.With(manage).Delete("/barcodes/{id}", h.deleteBarcode)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	cat, err := httpx.QueryUUID(r, "category_id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	brand, err := httpx.QueryUUID(r, "brand_id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	pg := pagination.FromRequest(r)
	out, meta, err := h.svc.List(r.Context(), ListFilter{Q: r.URL.Query().Get("q"), CategoryID: cat, BrandID: brand, Status: r.URL.Query().Get("status")}, pg)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OKMeta(w, out, meta)
}

func (h *Handler) lookup(w http.ResponseWriter, r *http.Request) {
	bc := r.URL.Query().Get("barcode")
	if bc == "" {
		h.Fail(w, r, apperr.BadRequest("barcode is required"))
		return
	}
	out, err := h.svc.Lookup(r.Context(), bc)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type barcodeReq struct {
	Barcode   string `json:"barcode" validate:"required,max=64"`
	Kind      string `json:"kind" validate:"omitempty,oneof=EAN13 INTERNAL UPC CODE128"`
	IsPrimary bool   `json:"is_primary"`
}

type variantReq struct {
	SKU           string           `json:"sku" validate:"required,max=64"`
	Name          string           `json:"name" validate:"max=120"`
	Attributes    json.RawMessage  `json:"attributes"`
	PurchasePrice decimal.Decimal  `json:"purchase_price"`
	Cost          decimal.Decimal  `json:"cost"`
	MinStock      decimal.Decimal  `json:"min_stock"`
	MaxStock      decimal.Decimal  `json:"max_stock"`
	ReorderPoint  decimal.Decimal  `json:"reorder_point"`
	SellingPrice  *decimal.Decimal `json:"selling_price"`
	Barcodes      []barcodeReq     `json:"barcodes" validate:"dive"`
}

func (v variantReq) input() VariantInput {
	bs := make([]BarcodeInput, len(v.Barcodes))
	for i, b := range v.Barcodes {
		bs[i] = BarcodeInput{Barcode: b.Barcode, Kind: b.Kind, IsPrimary: b.IsPrimary}
	}
	return VariantInput{SKU: v.SKU, Name: v.Name, Attributes: v.Attributes, PurchasePrice: v.PurchasePrice, Cost: v.Cost,
		MinStock: v.MinStock, MaxStock: v.MaxStock, ReorderPoint: v.ReorderPoint, SellingPrice: v.SellingPrice, Barcodes: bs}
}

type createRequest struct {
	Code          string       `json:"code" validate:"required,max=64"`
	Name          string       `json:"name" validate:"required,max=200"`
	ShortName     string       `json:"short_name" validate:"max=60"`
	CategoryID    *uuid.UUID   `json:"category_id"`
	BrandID       *uuid.UUID   `json:"brand_id"`
	UOMID         uuid.UUID    `json:"uom_id" validate:"required"`
	TaxCode       string       `json:"tax_code" validate:"max=16"`
	IsRFIDTracked bool         `json:"is_rfid_tracked"`
	Variants      []variantReq `json:"variants" validate:"required,min=1,dive"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	in := CreateInput{Code: req.Code, Name: req.Name, ShortName: req.ShortName, CategoryID: req.CategoryID, BrandID: req.BrandID, UOMID: req.UOMID,
		TaxCode: req.TaxCode, IsRFIDTracked: req.IsRFIDTracked}
	for _, v := range req.Variants {
		in.Variants = append(in.Variants, v.input())
	}
	out, err := h.svc.Create(r.Context(), in)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type updateRequest struct {
	Version       int32      `json:"version" validate:"required,min=1"`
	Name          *string    `json:"name" validate:"omitempty,max=200"`
	ShortName     *string    `json:"short_name" validate:"omitempty,max=60"`
	CategoryID    *uuid.UUID `json:"category_id"`
	BrandID       *uuid.UUID `json:"brand_id"`
	UOMID         *uuid.UUID `json:"uom_id"`
	TaxCode       *string    `json:"tax_code" validate:"omitempty,max=16"`
	IsRFIDTracked *bool      `json:"is_rfid_tracked"`
	Status        *string    `json:"status" validate:"omitempty,oneof=active inactive discontinued"`
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req updateRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Update(r.Context(), id, UpdateInput{Version: req.Version, Name: req.Name, ShortName: req.ShortName, CategoryID: req.CategoryID,
		BrandID: req.BrandID, UOMID: req.UOMID, TaxCode: req.TaxCode, IsRFIDTracked: req.IsRFIDTracked, Status: req.Status})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) addVariant(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req variantReq
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.AddVariant(r.Context(), id, req.input())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

func (h *Handler) addBarcode(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req barcodeReq
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.AddBarcode(r.Context(), id, BarcodeInput{Barcode: req.Barcode, Kind: req.Kind, IsPrimary: req.IsPrimary})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

func (h *Handler) deleteBarcode(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.svc.DeleteBarcode(r.Context(), id); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.NoContent(w)
}
