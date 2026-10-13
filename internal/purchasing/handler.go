package purchasing

import (
	"net/http"

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
	r.Route("/purchase-orders", func(r chi.Router) {
		r.With(g.AnyPerm(PermView, PermCreate, PermApprove, PermReceive)).Get("/", h.listPO)
		r.With(g.AnyPerm(PermView, PermCreate, PermApprove, PermReceive)).Get("/{id}", h.getPO)
		r.With(g.Perm(PermCreate)).Post("/", h.createPO)
		r.With(g.Perm(PermCreate)).Put("/{id}", h.updatePO)
		r.With(g.Perm(PermCreate)).Post("/{id}/submit", h.act(func(s *Service, r *http.Request, id uuid.UUID) (any, error) { return s.Submit(r.Context(), id) }))
		r.With(g.Perm(PermApprove)).Post("/{id}/approve", h.act(func(s *Service, r *http.Request, id uuid.UUID) (any, error) { return s.Approve(r.Context(), id) }))
		r.With(g.AnyPerm(PermCreate, PermApprove)).Post("/{id}/cancel", h.cancel)
		r.With(g.AnyPerm(PermReceive, PermApprove)).Post("/{id}/close", h.act(func(s *Service, r *http.Request, id uuid.UUID) (any, error) { return s.CloseShort(r.Context(), id) }))
	})
	r.Route("/goods-receipts", func(r chi.Router) {
		r.With(g.AnyPerm(PermView, PermReceive, PermReturn)).Get("/", h.listGRN)
		r.With(g.AnyPerm(PermView, PermReceive, PermReturn)).Get("/{id}", h.getGRN)
		r.With(g.Perm(PermReceive)).Post("/", h.receive)
	})
	r.Route("/purchase-returns", func(r chi.Router) {
		r.With(g.AnyPerm(PermView, PermReturn)).Get("/", h.listReturns)
		r.With(g.Perm(PermReturn)).Post("/", h.createReturn)
	})
}

func (h *Handler) listPO(w http.ResponseWriter, r *http.Request) {
	sid, e1 := httpx.QueryUUID(r, "store_id")
	sup, e2 := httpx.QueryUUID(r, "supplier_id")
	for _, e := range []error{e1, e2} {
		if e != nil {
			h.Fail(w, r, e)
			return
		}
	}
	out, meta, err := h.svc.ListPOs(r.Context(), POFilter{StoreID: sid, SupplierID: sup, Status: r.URL.Query().Get("status"), Q: r.URL.Query().Get("q")}, pagination.FromRequest(r))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OKMeta(w, out, meta)
}

func (h *Handler) getPO(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.GetPO(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) createPO(w http.ResponseWriter, r *http.Request) {
	var in POInput
	if err := httpx.Bind(r, &in); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.CreatePO(r.Context(), in)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

func (h *Handler) updatePO(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var in POInput
	if err := httpx.Bind(r, &in); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.UpdatePO(r.Context(), id, in)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) act(fn func(s *Service, r *http.Request, id uuid.UUID) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := httpx.PathUUID(r, "id")
		if err != nil {
			h.Fail(w, r, err)
			return
		}
		out, err := fn(h.svc, r, id)
		if err != nil {
			h.Fail(w, r, err)
			return
		}
		response.OK(w, out)
	}
}

type cancelRequest struct {
	Reason string `json:"reason" validate:"required,max=200"`
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req cancelRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Cancel(r.Context(), id, req.Reason)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) listGRN(w http.ResponseWriter, r *http.Request) {
	sid, e1 := httpx.QueryUUID(r, "store_id")
	po, e2 := httpx.QueryUUID(r, "po_id")
	for _, e := range []error{e1, e2} {
		if e != nil {
			h.Fail(w, r, e)
			return
		}
	}
	out, err := h.svc.ListGRNs(r.Context(), sid, po, pagination.FromRequest(r))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) getGRN(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.GetGRN(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) receive(w http.ResponseWriter, r *http.Request) {
	var in GRNInput
	if err := httpx.Bind(r, &in); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Receive(r.Context(), in)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

func (h *Handler) listReturns(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListReturns(r.Context(), pagination.FromRequest(r))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) createReturn(w http.ResponseWriter, r *http.Request) {
	var in ReturnInput
	if err := httpx.Bind(r, &in); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Return(r.Context(), in)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}
