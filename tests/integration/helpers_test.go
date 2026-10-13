// Package integration runs the API end-to-end against a real Postgres (TEST_DATABASE_URL).
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/ritel35k/backend/internal/app"
	"github.com/ritel35k/backend/internal/bootstrap"
	"github.com/ritel35k/backend/internal/platform/config"
	"github.com/ritel35k/backend/internal/platform/database"
)

var (
	testPool *pgxpool.Pool
	testApp  *app.App
	testSrv  *httptest.Server
	seq      atomic.Int64
)

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		fmt.Println("TEST_DATABASE_URL not set; skipping integration tests")
		os.Exit(0)
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	if err := database.MigrateReset(ctx, pool); err != nil {
		fmt.Println("reset:", err)
		os.Exit(1)
	}
	if err := database.MigrateUp(ctx, pool); err != nil {
		fmt.Println("migrate:", err)
		os.Exit(1)
	}
	if err := database.EnsurePartitions(ctx, pool, time.Now(), 2); err != nil {
		fmt.Println("partitions:", err)
		os.Exit(1)
	}
	if err := bootstrap.SyncPermissions(ctx, pool); err != nil {
		fmt.Println("perms:", err)
		os.Exit(1)
	}
	cfg := config.Config{Env: "test", JWTSecret: strings.Repeat("s", 40), AccessTTL: 15 * time.Minute, RefreshTTL: time.Hour, CORSOrigins: []string{"http://localhost"}, AuthRatePerMin: 100000, AuthRateBurst: 100000}
	testPool = pool
	testApp = app.New(cfg, pool, zerolog.Nop())
	testSrv = httptest.NewServer(testApp.Handler())
	code := m.Run()
	testSrv.Close()
	pool.Close()
	os.Exit(code)
}

type client struct {
	t     *testing.T
	token string
}

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Meta    json.RawMessage `json:"meta"`
	Error   *struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	} `json:"error"`
}

func (c *client) do(method, path string, body any) (int, envelope) {
	c.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, testSrv.URL+"/api/v1"+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var env envelope
	if resp.StatusCode != http.StatusNoContent {
		_ = json.NewDecoder(resp.Body).Decode(&env)
	}
	return resp.StatusCode, env
}

func (c *client) mustOK(method, path string, body any, into any) {
	c.t.Helper()
	st, env := c.do(method, path, body)
	if st >= 300 {
		msg := ""
		if env.Error != nil {
			msg = env.Error.Code + ": " + env.Error.Message + " " + string(env.Error.Details)
		}
		c.t.Fatalf("%s %s -> %d %s", method, path, st, msg)
	}
	if into != nil {
		if err := json.Unmarshal(env.Data, into); err != nil {
			c.t.Fatalf("decode %s: %v (%s)", path, err, env.Data)
		}
	}
}

func (c *client) wantErr(method, path string, body any, status int, code string) {
	c.t.Helper()
	st, env := c.do(method, path, body)
	got := ""
	if env.Error != nil {
		got = env.Error.Code
	}
	if st != status || (code != "" && got != code) {
		c.t.Fatalf("%s %s: want %d %s, got %d %s", method, path, status, code, st, got)
	}
}

type tenant struct {
	CompanyID string
	Owner     *client
	Email     string
	Password  string
	RootUnit  string
}

func uniq(prefix string) string { return fmt.Sprintf("%s%d", prefix, seq.Add(1)) }

func login(t *testing.T, email, pw string) *client {
	t.Helper()
	c := &client{t: t}
	var res struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	c.mustOK("POST", "/auth/login", map[string]string{"email": email, "password": pw}, &res)
	c.token = res.AccessToken
	return c
}

// newTenant bootstraps an isolated company and returns its logged-in owner.
func newTenant(t *testing.T) *tenant {
	t.Helper()
	code := strings.ToUpper(uniq("T"))
	email := strings.ToLower(code) + "@owner.test"
	res, err := bootstrap.CreateCompany(context.Background(), testPool, bootstrap.Input{CompanyCode: code, CompanyName: "Test " + code,
		OwnerEmail: email, OwnerName: "Owner", OwnerPassword: "owner-pass-123"})
	if err != nil {
		t.Fatal(err)
	}
	return &tenant{CompanyID: res.CompanyID.String(), Owner: login(t, email, "owner-pass-123"), Email: email, Password: "owner-pass-123", RootUnit: res.RootUnitID.String()}
}

type storeOut struct {
	Store struct {
		ID        string `json:"id"`
		OrgUnitID string `json:"org_unit_id"`
		Code      string `json:"code"`
		Path      string `json:"path"`
	} `json:"store"`
	Warehouse struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	} `json:"default_warehouse"`
}

func (tn *tenant) unit(t *testing.T, typ, code, parent string) string {
	t.Helper()
	var u struct {
		ID string `json:"id"`
	}
	tn.Owner.mustOK("POST", "/organization/units", map[string]any{"parent_id": parent, "type": typ, "code": code, "name": code}, &u)
	return u.ID
}

func (tn *tenant) store(t *testing.T, code, parent string) storeOut {
	t.Helper()
	body := map[string]any{"code": code, "name": "Toko " + code}
	if parent != "" {
		body["parent_org_unit_id"] = parent
	}
	var s storeOut
	tn.Owner.mustOK("POST", "/organization/stores", body, &s)
	return s
}

func roleID(t *testing.T, c *client, code string) string {
	t.Helper()
	var roles []struct {
		ID, Code string
	}
	c.mustOK("GET", "/roles", nil, &roles)
	for _, r := range roles {
		if r.Code == code {
			return r.ID
		}
	}
	t.Fatalf("role %s not found", code)
	return ""
}

// user creates a user with one role at one org unit ("" = company-wide) and returns their client.
func (tn *tenant) user(t *testing.T, role, orgUnit string) *client {
	t.Helper()
	email := strings.ToLower(uniq("u")) + "@x.test"
	as := []map[string]any{{"role_id": roleID(t, tn.Owner, role)}}
	if orgUnit != "" {
		as[0]["org_unit_id"] = orgUnit
	}
	tn.Owner.mustOK("POST", "/users", map[string]any{"email": email, "name": role, "password": "user-pass-123", "pin": "1234", "assignments": as}, nil)
	return login(t, email, "user-pass-123")
}
