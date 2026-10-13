// Package pos implements the point of sale: terminals, cart pricing (quote), atomic checkout,
// void, returns/refunds and receipts. A sale, its payments, the stock movements and the outbox
// event are written in ONE database transaction.
package pos

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
)

const (
	PermCreate        = "pos.transaction.create"
	PermVoid          = "pos.transaction.void"
	PermDiscount      = "pos.discount.manual"
	PermRefund        = "pos.refund.create"
	PermRefundApprove = "pos.refund.approve"
	PermView          = "pos.sale.view"
	PermTerminal      = "pos.terminal.manage"

	EventSaleCompleted = "SaleCompleted"
	EventSaleVoided    = "SaleVoided"
	EventSaleRefunded  = "SaleRefunded"
)

// Approver verifies a supervisor's PIN for a sensitive action (implemented by auth.Service).
type Approver interface {
	Approve(ctx context.Context, email, pin, permission string, storeID uuid.UUID) (uuid.UUID, error)
}

type Service struct {
	tx       *tx.Manager
	approver Approver
	now      func() time.Time
}

func NewService(t *tx.Manager, a Approver) *Service {
	return &Service{tx: t, approver: a, now: time.Now}
}

type ApprovalInput struct {
	ApproverEmail string `json:"approver_email" validate:"omitempty,email"`
	PIN           string `json:"pin"`
}

// requireApproval lets the action through when the caller already holds the permission in the store,
// otherwise demands a supervisor PIN. Returns the approving user, if any.
func (s *Service) requireApproval(ctx context.Context, p *access.Principal, storeID uuid.UUID, storePath, perm string, in *ApprovalInput) (*uuid.UUID, error) {
	if p.Grants.Can(perm, storePath) {
		return nil, nil
	}
	if in == nil || in.ApproverEmail == "" || in.PIN == "" {
		return nil, apperr.New(403, "APPROVAL_REQUIRED", "Supervisor approval required").WithDetails(map[string]string{"permission": perm})
	}
	id, err := s.approver.Approve(ctx, in.ApproverEmail, in.PIN, perm, storeID)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

var hundred = decimal.NewFromInt(100)
