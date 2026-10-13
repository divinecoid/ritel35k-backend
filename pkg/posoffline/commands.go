package posoffline

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Command types the edge queues while the cloud is unreachable and replays in order, once per ID.
const (
	CmdShiftOpen  = "shift_open"
	CmdSale       = "sale"
	CmdShiftClose = "shift_close"
)

// Results of applying one command.
const (
	StatusApplied   = "applied"
	StatusDuplicate = "duplicate" // already applied by an earlier attempt: acknowledged, nothing changed
	StatusRejected  = "rejected"  // can never succeed (bad terminal, malformed): dead-lettered at the edge, never retried
)

type Command struct {
	Type       string    `json:"type"`
	ID         uuid.UUID `json:"id"` // sale: client_txn_id; shift commands: client_shift_id
	OccurredAt time.Time `json:"occurred_at"`

	Sale       *SaleCommand       `json:"sale,omitempty"`
	ShiftOpen  *ShiftOpenCommand  `json:"shift_open,omitempty"`
	ShiftClose *ShiftCloseCommand `json:"shift_close,omitempty"`
}

type ShiftOpenCommand struct {
	CashierID   uuid.UUID       `json:"cashier_id"`
	OpeningCash decimal.Decimal `json:"opening_cash"`
}

type ShiftCloseCommand struct {
	ShiftRef
	CountedCash decimal.Decimal `json:"counted_cash"`
	Note        string          `json:"note"`
}

// ShiftRef names the shift a command belongs to: a shift the server already knows, or one opened offline.
type ShiftRef struct {
	ShiftID       *uuid.UUID `json:"shift_id,omitempty"`
	ClientShiftID *uuid.UUID `json:"client_shift_id,omitempty"`
}

type SalePayment struct {
	Method    string          `json:"method"`
	Amount    decimal.Decimal `json:"amount"`   // applied to the bill
	Tendered  decimal.Decimal `json:"tendered"` // handed over (cash) / charged
	Reference string          `json:"reference"`
}

// SaleCommand is a sale that already happened in the store: the goods are gone and the money is in the till, so the
// server records it as given and only FLAGS differences (offline exceptions) — it never rejects a real sale.
type SaleCommand struct {
	ShiftRef
	CashierID       uuid.UUID       `json:"cashier_id"`
	LocalNumber     string          `json:"local_number"` // the number printed on the offline receipt
	SnapshotVersion string          `json:"snapshot_version"`
	CustomerID      *uuid.UUID      `json:"customer_id,omitempty"`
	Lines           []Line          `json:"lines"`
	Subtotal        decimal.Decimal `json:"subtotal"`
	DiscountTotal   decimal.Decimal `json:"discount_total"`
	PromoDiscount   decimal.Decimal `json:"promo_discount"`
	TaxTotal        decimal.Decimal `json:"tax_total"`
	GrandTotal      decimal.Decimal `json:"grand_total"`
	Payments        []SalePayment   `json:"payments"`
	Change          decimal.Decimal `json:"change_amount"`
}

type SyncRequest struct {
	Commands     []Command `json:"commands"`
	AgentVersion string    `json:"agent_version"`
	Pending      int       `json:"pending"` // queue length at the edge (shown in the back office)
}

type CommandResult struct {
	ID             uuid.UUID  `json:"id"`
	Type           string     `json:"type"`
	Status         string     `json:"status"`
	Code           string     `json:"code,omitempty"`
	Message        string     `json:"message,omitempty"`
	SaleID         *uuid.UUID `json:"sale_id,omitempty"`
	ShiftID        *uuid.UUID `json:"shift_id,omitempty"`
	DocumentNumber string     `json:"document_number,omitempty"`
	Exceptions     []string   `json:"exceptions,omitempty"` // exception types raised for review
}

type SyncResponse struct {
	Results    []CommandResult `json:"results"`
	ServerTime time.Time       `json:"server_time"`
}

// Exception types (offline_exceptions.type).
const (
	ExPriceMismatch = "PRICE_MISMATCH" // the edge's prices/total differ from what the server computes for that moment
	ExNegativeStock = "NEGATIVE_STOCK" // the sale took stock below zero
	ExTagConflict   = "TAG_CONFLICT"   // an RFID tag in the sale was not sellable when synced
	ExLateSale      = "LATE_SALE"      // synced into a shift that was already closed
	ExUnpriceable   = "UNPRICEABLE"    // the server could not re-price the cart (item removed/inactive)
	ExMemberMissing = "MEMBER_MISSING" // the member no longer exists: no points credited
	ExPaymentMethod = "PAYMENT_METHOD" // a payment method that is disabled for the store now
	ExStaleSnapshot = "STALE_SNAPSHOT" // sold with a catalogue much older than the server's
)
