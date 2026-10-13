// Package access implements RBAC + organizational scope: permission catalog, grants and scope filters.
package access

import "strings"

// Permission codes. Phase-1 codes are enforced by handlers; the rest are seeded so roles can be
// designed up-front and later modules only need to start enforcing them.
const (
	OrganizationView        = "organization.view"
	OrganizationStoreCreate = "organization.store.create"
	OrganizationStoreUpdate = "organization.store.update"
	OrganizationUnitManage  = "organization.unit.manage"

	SettingsView   = "settings.view"
	SettingsManage = "settings.manage"

	UserView   = "user.view"
	UserManage = "user.manage"
	RoleView   = "role.view"
	RoleManage = "role.manage"

	WarehouseView   = "warehouse.view"
	WarehouseManage = "warehouse.manage"

	ProductView   = "product.view"
	ProductManage = "product.manage"

	SupplierView   = "supplier.view"
	SupplierManage = "supplier.manage"

	AuditView = "audit.view"

	PricingView   = "pricing.view"
	PricingManage = "pricing.manage"

	InventoryView   = "inventory.view"
	InventoryAdjust = "inventory.adjust"

	// Supervisor override at the terminal (PIN approval).
	ApprovalPIN = "approval.pin.approve"
)

type Def struct{ Code, Description string }

// Catalog is the full permission list synced into the permissions table on startup.
var Catalog = []Def{
	{OrganizationView, "View organization tree and stores"},
	{OrganizationStoreCreate, "Add a new store"},
	{OrganizationStoreUpdate, "Edit store details"},
	{OrganizationUnitManage, "Manage regions / business units"},
	{SettingsView, "View settings"},
	{SettingsManage, "Change settings overrides"},
	{UserView, "View users"},
	{UserManage, "Create / edit users and assign scopes"},
	{RoleView, "View roles"},
	{RoleManage, "Create / edit roles"},
	{WarehouseView, "View warehouses"},
	{WarehouseManage, "Manage warehouses"},
	{ProductView, "View products and master data"},
	{ProductManage, "Create / edit products and master data"},
	{SupplierView, "View suppliers"},
	{SupplierManage, "Manage suppliers"},
	{AuditView, "View audit logs"},
	{ApprovalPIN, "Approve sensitive POS actions with PIN"},

	// Forward-looking (enforced by later phases).
	{"pos.transaction.create", "Create POS transactions"},
	{"pos.transaction.void", "Void POS transactions"},
	{"pos.discount.manual", "Apply manual discount"},
	{"pos.refund.create", "Create refunds"},
	{"pos.refund.approve", "Approve refunds"},
	{"pos.shift.manage", "Open / close shifts"},
	{"pos.sale.view", "View sales and receipts"},
	{"pos.terminal.manage", "Manage POS terminals"},
	{"inventory.view", "View inventory"},
	{"inventory.adjust", "Adjust stock"},
	{"inventory.transfer", "Transfer stock"},
	{"inventory.count", "Stock opname"},
	{"inventory.transfer.approve", "Approve and ship stock transfers"},
	{"inventory.count.approve", "Approve stock count variances"},
	{"purchase_order.create", "Create purchase orders"},
	{"purchase_order.approve", "Approve purchase orders"},
	{"purchase_order.view", "View purchase orders and goods receipts"},
	{"purchase_return.create", "Return goods to suppliers"},
	{"goods_receipt.create", "Receive goods"},
	{"pricing.view", "View prices"},
	{"pricing.manage", "Manage price lists"},
	{"promotion.manage", "Manage promotions"},
	{"customer.view", "View customers"},
	{"customer.manage", "Edit customers, adjust points, configure membership levels"},
	{"customer.create", "Register customers / members"},
	{"report.sales.view", "View sales reports"},
	{"report.profit.view", "View profit reports"},
	{"report.inventory.view", "View inventory reports"},
	{"dashboard.view", "View dashboard"},
	{"rfid.view", "View RFID tags, readers and stock reconciliation"},
	{"rfid.manage", "Register tags, change tag status, enroll and manage RFID readers"},
	{"security.view", "View security events"},
	{"security.resolve", "Resolve security events"},
}

// DefaultRole is a system role seeded for every company. Patterns support a trailing ".*".
type DefaultRole struct {
	Code, Name string
	Patterns   []string
}

var DefaultRoles = []DefaultRole{
	{"owner", "Owner", []string{"*"}},
	{"head_office", "Head Office", []string{
		"organization.view", "organization.store.*", "organization.unit.manage", "settings.*", "user.*", "role.view",
		"warehouse.*", "product.*", "supplier.*", "audit.view", "pricing.*", "promotion.*", "customer.*",
		"report.*", "dashboard.view", "inventory.*", "purchase_order.*", "purchase_return.*", "goods_receipt.*", "security.*", "rfid.*",
	}},
	{"regional_manager", "Regional Manager", []string{
		"organization.view", "settings.view", "user.view", "warehouse.view", "product.view", "supplier.view",
		"report.*", "dashboard.view", "inventory.*", "purchase_order.approve", "purchase_order.view", "pos.refund.approve", "security.*", "audit.view", "rfid.view",
		"approval.pin.approve",
	}},
	{"store_manager", "Store Manager", []string{
		"organization.view", "organization.store.update", "settings.view", "user.view", "warehouse.view", "product.view",
		"supplier.view", "pricing.view", "report.sales.view", "report.inventory.view", "dashboard.view", "inventory.*", "pos.*",
		"purchase_order.create", "purchase_order.view", "purchase_return.create", "goods_receipt.create", "approval.pin.approve", "security.view", "customer.*", "rfid.*",
	}},
	{"supervisor", "Supervisor", []string{
		"organization.view", "product.view", "inventory.view", "pos.*", "pricing.view", "approval.pin.approve", "dashboard.view",
		"report.sales.view", "customer.view", "customer.create",
	}},
	{"cashier", "Cashier", []string{
		"organization.view", "product.view", "pos.transaction.create", "pos.shift.manage", "pos.refund.create",
		"pos.sale.view", "customer.view", "customer.create", "inventory.view",
	}},
	{"warehouse_staff", "Warehouse Staff", []string{
		"organization.view", "product.view", "warehouse.view", "inventory.view", "inventory.transfer", "inventory.count",
		"goods_receipt.create", "purchase_order.view", "rfid.*",
	}},
	{"purchasing", "Purchasing", []string{
		"organization.view", "product.view", "supplier.*", "purchase_order.*", "purchase_return.*", "goods_receipt.*", "inventory.view", "warehouse.view",
		"report.inventory.view",
	}},
	{"finance", "Finance", []string{
		"organization.view", "report.*", "dashboard.view", "audit.view", "supplier.view", "product.view", "purchase_order.view",
	}},
	{"loss_prevention", "Loss Prevention", []string{
		"organization.view", "security.*", "audit.view", "report.sales.view", "dashboard.view", "inventory.view", "rfid.*",
	}},
	{"auditor", "Auditor", []string{
		"organization.view", "settings.view", "user.view", "role.view", "warehouse.view", "product.view", "supplier.view",
		"audit.view", "report.*", "dashboard.view", "inventory.view", "security.view", "purchase_order.view", "rfid.view",
	}},
}

// Expand resolves patterns ("*", "pos.*", exact codes) against the catalog.
func Expand(patterns []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range Catalog {
		for _, p := range patterns {
			if match(p, d.Code) && !seen[d.Code] {
				seen[d.Code] = true
				out = append(out, d.Code)
			}
		}
	}
	return out
}

func match(pattern, code string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasSuffix(pattern, ".*"):
		return strings.HasPrefix(code, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == code
	}
}

func IsKnown(code string) bool {
	for _, d := range Catalog {
		if d.Code == code {
			return true
		}
	}
	return false
}
