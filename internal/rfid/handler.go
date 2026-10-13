package rfid

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/pkg/pagination"
)

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.Route("/rfid", func(r chi.Router) {
		r.With(g.Perm(PermView)).Get("/tags", h.list)
		r.With(g.Perm(PermView)).Get("/tags/{epc}", h.get)
		r.With(g.Perm(PermManage)).Post("/tags/register", h.register)
		r.With(g.Perm(PermManage)).Post("/tags/status", h.status)
		r.With(g.Perm(PermManage)).Post("/tags/move", h.move)
		r.With(g.Perm(PermView)).Get("/variance", h.variance)

		r.With(g.Perm(PermView)).Get("/readers", h.readers)
		r.With(g.Perm(PermManage)).Post("/readers", h.createReader)
		r.With(g.Perm(PermManage)).Patch("/readers/{id}", h.updateReader)
		r.With(g.Perm(PermManage)).Post("/readers/{id}/rotate-token", h.rotate)
	})
}

func (h *Handler) filter(r *http.Request) (TagFilter, error) {
	var f TagFilter
	var err error
	if f.StoreID, err = httpx.QueryUUID(r, "store_id"); err != nil {
		return f, err
	}
	if f.VariantID, err = httpx.QueryUUID(r, "variant_id"); err != nil {
		return f, err
	}
	q := r.URL.Query()
	f.UnderPath, f.Status, f.Q = q.Get("under_path"), q.Get("status"), q.Get("q")
	return f, nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	f, err := h.filter(r)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	rows, meta, err := h.svc.List(r.Context(), f, pagination.FromRequest(r))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OKMeta(w, rows, meta)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.Get(r.Context(), chi.URLParam(r, "epc"))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type registerRequest struct {
	VariantID uuid.UUID  `json:"variant_id" validate:"required"`
	StoreID   *uuid.UUID `json:"store_id"`
	Status    string     `json:"status" validate:"omitempty,oneof=REGISTERED IN_STOCK ON_SHELF"`
	Location  string     `json:"location" validate:"max=80"`
	EPCs      []string   `json:"epcs" validate:"required,min=1,max=2000"`
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Register(r.Context(), RegisterInput{VariantID: req.VariantID, StoreID: req.StoreID, Status: req.Status, Location: req.Location, EPCs: req.EPCs})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type statusRequest struct {
	EPCs   []string `json:"epcs" validate:"required,min=1,max=2000"`
	Status string   `json:"status" validate:"required,oneof=IN_STOCK ON_SHELF RESERVED DAMAGED LOST"`
	Reason string   `json:"reason" validate:"max=300"`
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	var req statusRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	n, err := h.svc.ChangeStatus(r.Context(), ChangeInput{EPCs: req.EPCs, Status: req.Status, Reason: req.Reason})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, map[string]int{"changed": n})
}

type moveRequest struct {
	EPCs     []string   `json:"epcs" validate:"required,min=1,max=2000"`
	StoreID  *uuid.UUID `json:"store_id"`
	Location string     `json:"location" validate:"max=80"`
}

func (h *Handler) move(w http.ResponseWriter, r *http.Request) {
	var req moveRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	n, err := h.svc.Move(r.Context(), MoveInput{EPCs: req.EPCs, StoreID: req.StoreID, Location: req.Location})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, map[string]int{"moved": n})
}

func (h *Handler) variance(w http.ResponseWriter, r *http.Request) {
	f, err := h.filter(r)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := h.svc.Variance(r.Context(), f, r.URL.Query().Get("only_diff") != "false", limit)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, rows)
}

func (h *Handler) readers(w http.ResponseWriter, r *http.Request) {
	sid, err := httpx.QueryUUID(r, "store_id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	rows, err := h.svc.Readers(r.Context(), sid)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, rows)
}

type readerRequest struct {
	StoreID uuid.UUID `json:"store_id" validate:"required"`
	Code    string    `json:"code" validate:"required,max=30"`
	Name    string    `json:"name" validate:"required,max=80"`
	Kind    string    `json:"kind" validate:"required,oneof=gate pos shelf handheld"`
}

func (h *Handler) createReader(w http.ResponseWriter, r *http.Request) {
	var req readerRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.CreateReader(r.Context(), ReaderInput{StoreID: req.StoreID, Code: req.Code, Name: req.Name, Kind: req.Kind})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type readerUpdate struct {
	Name   *string `json:"name" validate:"omitempty,max=80"`
	Status *string `json:"status" validate:"omitempty,oneof=active inactive"`
}

func (h *Handler) updateReader(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req readerUpdate
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.UpdateReader(r.Context(), id, req.Name, req.Status)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) rotate(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	tok, err := h.svc.RotateToken(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, map[string]string{"token": tok})
}
