package integration

import (
	"context"
	"testing"
)

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	tn := newTenant(t)
	c := &client{t: t}
	var t1 tokens
	c.mustOK("POST", "/auth/login", map[string]string{"email": tn.Email, "password": tn.Password}, &t1)

	var t2 tokens
	c.mustOK("POST", "/auth/refresh", map[string]string{"refresh_token": t1.RefreshToken}, &t2)
	if t2.RefreshToken == t1.RefreshToken {
		t.Fatal("refresh token must rotate")
	}
	// replaying the old token = theft signal → whole family revoked
	c.wantErr("POST", "/auth/refresh", map[string]string{"refresh_token": t1.RefreshToken}, 401, "UNAUTHENTICATED")
	c.wantErr("POST", "/auth/refresh", map[string]string{"refresh_token": t2.RefreshToken}, 401, "UNAUTHENTICATED")
}

func TestLoginRules(t *testing.T) {
	tn := newTenant(t)
	c := &client{t: t}
	c.wantErr("POST", "/auth/login", map[string]string{"email": tn.Email, "password": "wrong-password"}, 401, "UNAUTHENTICATED")
	c.wantErr("POST", "/auth/login", map[string]string{"email": "nobody@x.test", "password": "whatever123"}, 401, "UNAUTHENTICATED")
	c.wantErr("GET", "/organization/stores", nil, 401, "UNAUTHENTICATED")

	var me struct {
		User   struct{ Email string } `json:"user"`
		Grants map[string]struct {
			All bool `json:"all"`
		} `json:"grants"`
	}
	tn.Owner.mustOK("GET", "/auth/me", nil, &me)
	if me.User.Email != tn.Email || !me.Grants["report.sales.view"].All {
		t.Fatalf("me: %+v", me)
	}

	// password change revokes sessions and takes effect
	tn.Owner.mustOK("POST", "/auth/change-password", map[string]string{"old_password": tn.Password, "new_password": "brand-new-pass-1"}, nil)
	c.wantErr("POST", "/auth/login", map[string]string{"email": tn.Email, "password": tn.Password}, 401, "")
	login(t, tn.Email, "brand-new-pass-1")
}

func TestSupervisorPINApproval(t *testing.T) {
	tn := newTenant(t)
	jabo := tn.unit(t, "region", "JABO", tn.RootUnit)
	a := tn.store(t, "ST-A", jabo)
	b := tn.store(t, "ST-B", tn.RootUnit)
	cashier := tn.user(t, "cashier", a.Store.OrgUnitID)
	sup := tn.user(t, "supervisor", a.Store.OrgUnitID)
	_ = sup
	var supEmail string
	_ = testPool.QueryRow(context.Background(), `select email from users where company_id=$1 and name='supervisor'`, tn.CompanyID).Scan(&supEmail)

	body := func(pin, perm, store string) map[string]any {
		return map[string]any{"approver_email": supEmail, "pin": pin, "permission": perm, "store_id": store}
	}
	var ap struct{ ApproverName string }
	cashier.mustOK("POST", "/auth/pin/approve", body("1234", "pos.discount.manual", a.Store.ID), &ap)
	cashier.wantErr("POST", "/auth/pin/approve", body("9999", "pos.discount.manual", a.Store.ID), 403, "APPROVAL_DENIED")
	// supervisor has no authority in a store outside their scope; cashier cannot even see it
	cashier.wantErr("POST", "/auth/pin/approve", body("1234", "pos.discount.manual", b.Store.ID), 404, "NOT_FOUND")
	// permission the supervisor lacks
	cashier.wantErr("POST", "/auth/pin/approve", body("1234", "user.manage", a.Store.ID), 403, "APPROVAL_DENIED")
}

func TestDelegationCannotEscalate(t *testing.T) {
	tn := newTenant(t)
	jabo := tn.unit(t, "region", "JABO", tn.RootUnit)
	a := tn.store(t, "ST-A", jabo)
	tn.store(t, "ST-B", jabo)
	// head_office-like user managing users only inside JABO
	roleCode := "ops_" + uniq("r")
	tn.Owner.mustOK("POST", "/roles", map[string]any{"code": roleCode, "name": "Ops", "permissions": []string{"user.view", "user.manage", "organization.view", "product.view", "pos.transaction.create", "pos.shift.manage", "pos.refund.create", "pos.sale.view", "customer.view", "customer.create", "inventory.view"}}, nil)
	ops := tn.user(t, roleCode, jabo)

	// can grant a subset of what they hold, inside their scope
	ops.mustOK("POST", "/users", map[string]any{"email": uniq("c") + "@x.test", "name": "c", "password": "cashier-pass-1",
		"assignments": []map[string]any{{"role_id": roleID(t, tn.Owner, "cashier"), "org_unit_id": a.Store.OrgUnitID}}}, nil)
	// cannot grant a role containing permissions they lack (store_manager has inventory.*)
	ops.wantErr("POST", "/users", map[string]any{"email": uniq("d") + "@x.test", "name": "d", "password": "cashier-pass-1",
		"assignments": []map[string]any{{"role_id": roleID(t, tn.Owner, "store_manager"), "org_unit_id": a.Store.OrgUnitID}}}, 403, "FORBIDDEN")
	// cannot grant company-wide
	ops.wantErr("POST", "/users", map[string]any{"email": uniq("e") + "@x.test", "name": "e", "password": "cashier-pass-1",
		"assignments": []map[string]any{{"role_id": roleID(t, tn.Owner, "cashier")}}}, 403, "FORBIDDEN")
	// cannot grant outside scope
	other := tn.unit(t, "region", "OTHER", tn.RootUnit)
	ops.wantErr("POST", "/users", map[string]any{"email": uniq("f") + "@x.test", "name": "f", "password": "cashier-pass-1",
		"assignments": []map[string]any{{"role_id": roleID(t, tn.Owner, "cashier"), "org_unit_id": other}}}, 403, "FORBIDDEN")
}
