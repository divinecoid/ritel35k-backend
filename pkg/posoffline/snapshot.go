package posoffline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/pkg/promoengine"
)

// Snapshot is everything a store terminal needs to price and sell without the cloud, resolved for ONE store: the
// prices already walk the org tree (company → region → store) so the edge needs no tree, and the version is a hash
// of the content so an unchanged catalogue is never re-downloaded.
type Snapshot struct {
	Version     string    `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`

	StoreID      uuid.UUID `json:"store_id"`
	StoreCode    string    `json:"store_code"`
	StoreName    string    `json:"store_name"`
	Address      string    `json:"address"`
	Timezone     string    `json:"timezone"`
	TerminalID   uuid.UUID `json:"terminal_id"`
	TerminalCode string    `json:"terminal_code"`

	Settings   Settings                `json:"settings"`
	Items      []Item                  `json:"items"`
	PriceLists []PriceList             `json:"price_lists"`
	Prices     []Price                 `json:"prices"`
	Promotions []promoengine.Promotion `json:"promotions"`
	Members    []Member                `json:"members"`
}

type Settings struct {
	PriceIncludesTax   bool            `json:"price_includes_tax"`
	PPNPercent         decimal.Decimal `json:"ppn_percent"`
	MaxDiscountPercent decimal.Decimal `json:"max_discount_percent"`
	PaymentMethods     []string        `json:"payment_methods"`
	AllowNegativeStock bool            `json:"allow_negative_stock"`
	LoyaltyEnabled     bool            `json:"loyalty_enabled"`
	EarnPerAmount      decimal.Decimal `json:"earn_per_amount"`
	ReceiptHeader      string          `json:"receipt_header"`
	ReceiptFooter      string          `json:"receipt_footer"`
}

type Item struct {
	VariantID     uuid.UUID  `json:"variant_id"`
	SKU           string     `json:"sku"`
	Name          string     `json:"name"` // product + variant
	Barcodes      []string   `json:"barcodes"`
	TaxCode       string     `json:"tax_code"`
	AllowsDecimal bool       `json:"allows_decimal"`
	CategoryID    *uuid.UUID `json:"category_id"`
	BrandID       *uuid.UUID `json:"brand_id"`
	RFIDTracked   bool       `json:"rfid_tracked"`
}

type PriceList struct {
	ID        uuid.UUID  `json:"id"`
	IsDefault bool       `json:"is_default"`
	LevelID   *uuid.UUID `json:"level_id"`
}

// Price is one price row from an org unit on the store's path. Depth is nlevel(path): deeper (nearer the store) wins.
type Price struct {
	VariantID  uuid.UUID       `json:"variant_id"`
	ListID     uuid.UUID       `json:"list_id"`
	MinQty     decimal.Decimal `json:"min_qty"`
	Price      decimal.Decimal `json:"price"`
	Depth      int             `json:"depth"`
	SourceName string          `json:"source_name"`
	From       time.Time       `json:"from"`
	To         *time.Time      `json:"to,omitempty"`
}

type Member struct {
	ID             uuid.UUID       `json:"id"`
	MemberNo       string          `json:"member_no"`
	Name           string          `json:"name"`
	Phone          string          `json:"phone"`
	LevelID        uuid.UUID       `json:"level_id"`
	LevelName      string          `json:"level_name"`
	EarnMultiplier decimal.Decimal `json:"earn_multiplier"`
	PointsBalance  int64           `json:"points_balance"`
}

// ComputeVersion hashes the content (not the version/time fields) so identical catalogues share a version.
func (s *Snapshot) ComputeVersion() string {
	c := *s
	c.Version, c.GeneratedAt = "", time.Time{}
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
