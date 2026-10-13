package auth

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/pkg/apperr"
)

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

// PublicRoutes are unauthenticated (login/refresh/logout); mount behind a rate limiter.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Post("/auth/login", h.login)
	r.Post("/auth/refresh", h.refresh)
	r.Post("/auth/logout", h.logout)
}

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.Get("/auth/me", h.me)
	r.Post("/auth/change-password", h.changePassword)
	r.Post("/auth/pin", h.setPIN)
	r.Post("/auth/pin/approve", h.approve)
}

type loginRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	res, err := h.svc.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, res)
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	res, err := h.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, res)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.svc.Logout(r.Context(), req.RefreshToken); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.NoContent(w)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	me, err := h.svc.Me(r.Context())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, me)
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password" validate:"required"`
	NewPassword string `json:"new_password" validate:"required"`
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.svc.ChangePassword(r.Context(), req.OldPassword, req.NewPassword); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.NoContent(w)
}

type setPINRequest struct {
	Password string `json:"password" validate:"required"`
	PIN      string `json:"pin" validate:"required"`
}

func (h *Handler) setPIN(w http.ResponseWriter, r *http.Request) {
	var req setPINRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.svc.SetPIN(r.Context(), req.Password, req.PIN); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.NoContent(w)
}

type approveRequest struct {
	ApproverEmail string    `json:"approver_email" validate:"required,email"`
	PIN           string    `json:"pin" validate:"required"`
	Permission    string    `json:"permission" validate:"required"`
	StoreID       uuid.UUID `json:"store_id" validate:"required"`
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	var req approveRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	if !access.IsKnown(req.Permission) {
		h.Fail(w, r, errUnknownPerm)
		return
	}
	res, err := h.svc.ApproveWithPIN(r.Context(), req.ApproverEmail, req.PIN, req.Permission, req.StoreID)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, res)
}

var errUnknownPerm = apperr.Validation(map[string]string{"permission": "unknown permission"})
