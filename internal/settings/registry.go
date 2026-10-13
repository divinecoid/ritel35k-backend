// Package settings implements inherited configuration: company → region → store, most specific wins.
package settings

import (
	"encoding/json"
	"fmt"
	"sort"
)

type Type string

const (
	String      Type = "string"
	Int         Type = "int"
	Number      Type = "number"
	Bool        Type = "bool"
	StringArray Type = "string_array"
)

type Def struct {
	Key         string `json:"key"`
	Type        Type   `json:"type"`
	Default     any    `json:"default"`
	Group       string `json:"group"`
	Description string `json:"description"`
}

// Registry is the closed set of known keys. Unknown keys are rejected so typos can't silently
// create dead config. Adding a key = adding a line here (not a migration, not a store change).
var Registry = []Def{
	{"pos.max_discount_percent", Number, 20, "POS", "Cashier discount above this % needs supervisor PIN"},
	{"pos.refund_approval_threshold", Number, 1000000, "POS", "Refund above this amount needs manager approval"},
	{"pos.allow_negative_stock", Bool, true, "POS", "Allow selling when system stock is zero/negative"},
	{"pos.void_alert_per_day", Int, 5, "POS", "Void count per cashier per day that raises a fraud alert"},
	{"payment.methods", StringArray, []string{"CASH", "QRIS", "DEBIT", "CREDIT", "EWALLET"}, "Payment", "Enabled payment methods"},
	{"tax.ppn_percent", Number, 11, "Tax", "VAT percentage"},
	{"tax.price_includes_tax", Bool, true, "Tax", "Selling prices already include tax"},
	{"receipt.header", String, "", "Receipt", "Receipt header text"},
	{"receipt.footer", String, "Terima kasih atas kunjungan Anda", "Receipt", "Receipt footer text"},
	{"inventory.default_warehouse_name", String, "Gudang Toko", "Inventory", "Name of the warehouse auto-created for a new store"},
	{"numbering.format.sale", String, "SO/{STORE}/{YYYY}/{MM}/{SEQ:6}", "Numbering", "Sales document number format"},
	{"numbering.format.sale_return", String, "RT/{STORE}/{YYYY}/{MM}/{SEQ:6}", "Numbering", "Sales return number format"},
	{"purchasing.po_approval_threshold", Number, 5000000, "Purchasing", "PO total at or below this is auto-approved when submitted; above needs an approver"},
	{"purchasing.over_receipt_percent", Number, 0, "Purchasing", "Tolerance (%) for receiving more than ordered"},
	{"inventory.count_self_approve_limit", Number, 1000000, "Inventory", "Stock-count variance value above this must be approved by someone other than the counter"},
	{"notify.refund_amount", Number, 500000, "Notifications", "A refund at or above this amount notifies managers (0 = every refund)"},
	{"notify.shift_variance", Number, 10000, "Notifications", "A closed shift whose cash variance exceeds this notifies managers"},
	{"rfid.require_tag_at_pos", Bool, false, "RFID", "RFID-tracked products must be sold by scanning their tag (not by barcode)"},
	{"rfid.report_unknown_tags", Bool, false, "RFID", "Raise a security event when a gate reads a tag the system does not know (usually a customer's own item)"},
	{"rfid.local_alarm_on_unknown", Bool, false, "RFID", "The store gate sounds its local alarm for unknown tags too"},
	{"rfid.sold_grace_seconds", Int, 120, "RFID", "A tag sold up to this long after it was read at the gate still counts as paid (clock skew, slow checkout)"},
	{"security.discount_percent", Number, 30, "Security", "A manual discount above this % of the subtotal is flagged to loss prevention"},
	{"offline.price_alert_amount", Number, 10000, "Offline POS", "An offline sale whose total differs from the server's recomputation by at least this much is raised to loss prevention"},
	{"loyalty.enabled", Bool, true, "Loyalty", "Members earn and redeem points"},
	{"loyalty.earn_per_amount", Number, 10000, "Loyalty", "Rupiah spent for 1 point (before the level multiplier)"},
	{"loyalty.redeem_value", Number, 100, "Loyalty", "Rupiah value of 1 point when redeemed"},
	{"loyalty.min_redeem_points", Int, 100, "Loyalty", "Minimum points per redemption"},
	{"loyalty.max_redeem_percent", Number, 50, "Loyalty", "Max share of a bill that points may pay (%)"},
	{"loyalty.expiry_days", Int, 365, "Loyalty", "Points expire this many days after earning (0 = never)"},
	{"numbering.format.member", String, "MBR-{SEQ:6}", "Numbering", "Member number format"},
	{"numbering.format.purchase_return", String, "PRT/{STORE}/{YYYY}/{MM}/{SEQ:6}", "Numbering", "Purchase return number format"},
	{"numbering.format.purchase_order", String, "PO/{STORE}/{YYYY}/{MM}/{SEQ:6}", "Numbering", "Purchase order number format"},
	{"numbering.format.goods_receipt", String, "GRN/{STORE}/{YYYY}/{MM}/{SEQ:6}", "Numbering", "Goods receipt number format"},
	{"numbering.format.stock_transfer", String, "TRF/{STORE}/{YYYY}/{MM}/{SEQ:6}", "Numbering", "Stock transfer number format"},
	{"numbering.format.stock_adjustment", String, "ADJ/{STORE}/{YYYY}/{MM}/{SEQ:6}", "Numbering", "Stock adjustment number format"},
	{"numbering.format.stock_count", String, "OPN/{STORE}/{YYYY}/{MM}/{SEQ:6}", "Numbering", "Stock count number format"},
}

var byKey = func() map[string]Def {
	m := make(map[string]Def, len(Registry))
	for _, d := range Registry {
		m[d.Key] = d
	}
	return m
}()

func Lookup(key string) (Def, bool) { d, ok := byKey[key]; return d, ok }

func Keys() []string {
	ks := make([]string, 0, len(byKey))
	for k := range byKey {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Validate checks raw JSON against the key's declared type and returns it normalized.
func Validate(def Def, raw json.RawMessage) (json.RawMessage, error) {
	switch def.Type {
	case String:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s must be a string", def.Key)
		}
		return json.Marshal(v)
	case Int:
		var v int64
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s must be an integer", def.Key)
		}
		return json.Marshal(v)
	case Number:
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s must be a number", def.Key)
		}
		return json.Marshal(v)
	case Bool:
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s must be a boolean", def.Key)
		}
		return json.Marshal(v)
	case StringArray:
		var v []string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s must be an array of strings", def.Key)
		}
		return json.Marshal(v)
	}
	return nil, fmt.Errorf("unsupported type %q", def.Type)
}
