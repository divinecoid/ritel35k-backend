// Package rfid manages RFID tags (status machine, registration, sale/void/return coupling), readers and their device
// credentials, and the edge-facing API (event batches, delta snapshot, heartbeat) including the gate decision.
package rfid

import (
	"regexp"
	"strings"

	"github.com/ritel35k/backend/pkg/apperr"
)

// Tag statuses.
const (
	Registered = "REGISTERED" // encoded / assigned to a product, not yet counted into stock
	InStock    = "IN_STOCK"   // back room or warehouse
	OnShelf    = "ON_SHELF"   // sales floor
	Reserved   = "RESERVED"   // held for a customer / display
	Sold       = "SOLD"       // paid for: allowed through the gate
	Returned   = "RETURNED"   // came back through a refund; sellable again
	Damaged    = "DAMAGED"
	Lost       = "LOST"
)

var AllStatuses = []string{Registered, InStock, OnShelf, Reserved, Sold, Returned, Damaged, Lost}

// manual lists the status changes a person may make by hand. SOLD is reachable only through checkout, and a tag
// leaves SOLD only through void (back on the shelf) or a return — never through this table.
var manual = map[string][]string{
	Registered: {InStock, OnShelf, Damaged, Lost},
	InStock:    {OnShelf, Reserved, Damaged, Lost},
	OnShelf:    {InStock, Reserved, Damaged, Lost},
	Reserved:   {InStock, OnShelf, Damaged, Lost},
	Returned:   {InStock, OnShelf, Damaged, Lost},
	Lost:       {InStock, OnShelf}, // found again
	Damaged:    {},                 // written off
	Sold:       {},
}

// CanChange reports whether a person may move a tag from one status to another.
func CanChange(from, to string) bool {
	for _, t := range manual[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Sellable tags may be sold at a POS of their own store.
func Sellable(status string) bool {
	return status == InStock || status == OnShelf || status == Returned
}

// Present tags are physically expected in the store (they count towards stock).
func Present(status string) bool {
	return status == InStock || status == OnShelf || status == Reserved || status == Returned
}

var epcRe = regexp.MustCompile(`^[0-9A-F]{8,64}$`)

// NormalizeEPC upper-cases, strips an optional 0x prefix and spaces/colons, and validates the hex form.
func NormalizeEPC(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "0X")
	s = strings.NewReplacer(" ", "", ":", "", "-", "").Replace(s)
	if !epcRe.MatchString(s) {
		return "", apperr.Validation(map[string]string{"epc": "must be 8-64 hex characters"})
	}
	return s, nil
}

// LooksLikeEPC is for scan inputs that may be a barcode or a tag: 24 hex digits is an EPC-96, which no barcode is.
func LooksLikeEPC(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= 16 && epcRe.MatchString(strings.ToUpper(s))
}
