package billing_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"101cloud/backend/internal/auth"
	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
	migrations "101cloud/backend/sql"
	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

type billingEventsMySQLFixture struct {
	ctx    context.Context
	db     *gorm.DB
	router *gin.Engine
	tokens map[uint64]string
}

type billingEventsResponse struct {
	AsOf     time.Time        `json:"as_of"`
	Timezone string           `json:"timezone"`
	Period   string           `json:"period"`
	From     time.Time        `json:"from"`
	To       time.Time        `json:"to"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
	Total    int64            `json:"total"`
	Items    []map[string]any `json:"items"`
}

func newBillingEventsMySQLFixture(t *testing.T) *billingEventsMySQLFixture {
	t.Helper()
	// Match report_admission_mysql_test: use real sessions in an external test
	// package, avoiding auth's dependency on billing becoming an import cycle.
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated billing event regression")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	owner, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "ci_billing_events_" + hex.EncodeToString(nonce[:])
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if _, err = owner.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Exec("DROP DATABASE `" + name + "`") })
	cfg.DBName = name
	db, err := platform.OpenDB(platform.Config{DSN: cfg.FormatDSN()})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	// Up embeds and applies every migration, including the event migration;
	// assert its receipt rather than silently relying on init.sql alone.
	if err = migrations.Up(ctx, db); err != nil {
		t.Fatal(err)
	}
	var applied int64
	if err = db.Table("backend_schema_migrations").Where("name = ?", "migrations/20261109_20_billing_events.sql").Count(&applied).Error; err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatal("billing event migration was not applied")
	}
	for _, statement := range []string{
		`INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'events-admin','unused-hash','admin','active'),(2,'events-owner','unused-hash','user','active'),(3,'events-no-wallet','unused-hash','user','active')`,
		`INSERT INTO credit_policy_versions(id,code,version,daily_allowance_pt,bonus_cap_pt,timezone,reset_time,billing_window_seconds,status,effective_at,created_by_user_id,daily_validity_seconds,shutdown_grace_seconds,grace_charge_mode,restore_action,storage_charge_mode,offline_charge_mode) VALUES(1,'events-credit','1',0,0,'Asia/Shanghai','06:30:00',60,'active','2000-01-01',1,86400,600,'waived','cancel_pending_stop','running_only','defer')`,
		`INSERT INTO credit_accounts(id,user_id,credit_policy_version_id,status) VALUES(1,2,1,'active'),(2,1,1,'active')`,
		`UPDATE credit_accounts SET revision=7 WHERE id=1`,
		`INSERT INTO credit_buckets(id,account_id,source_type,business_key,granted_pt,remaining_pt,reserved_pt,valid_from,expires_at) VALUES(1,1,'admin','events-expired-grant',10,10,0,'2000-01-01','2000-01-02')`,
		`INSERT INTO credit_ledger(account_id,bucket_id,business_key,entry_type,amount_delta_pt,reserved_delta_pt,remaining_after_pt,reserved_after_pt) VALUES(1,1,'events-expired-grant','grant',10,0,10,0)`,
	} {
		if err = db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	f := &billingEventsMySQLFixture{ctx: ctx, db: db, tokens: make(map[uint64]string)}
	for _, id := range []uint64{1, 2, 3} {
		var secret [32]byte
		if _, err = rand.Read(secret[:]); err != nil {
			t.Fatal(err)
		}
		token := base64.RawURLEncoding.EncodeToString(secret[:])
		f.tokens[id] = token
		hash := sha256.Sum256([]byte(token))
		if err = db.Exec("INSERT INTO user_sessions(user_id,token_hash,expires_at) VALUES(?,?,?)", id, hash[:], time.Now().Add(time.Hour)).Error; err != nil {
			t.Fatal(err)
		}
	}
	a := platform.New(db, platform.Config{Origin: "http://events.local"})
	r := gin.New()
	r.Use(platform.RequestMiddleware())
	member := r.Group("/api/v1", auth.Middleware(a), auth.CSRF(a), a.Mutations())
	billing.Register(a, member, r.Group("/api/v1/admin", platform.Admin()))
	f.router = r
	return f
}

func (f *billingEventsMySQLFixture) call(t *testing.T, query, token string, want int) billingEventsResponse {
	t.Helper()
	before := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/billing/events"+query, nil).WithContext(f.ctx)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "101cloud_session", Value: token})
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	after := time.Now()
	if w.Code != want {
		t.Fatalf("events%s: status=%d want=%d body=%s", query, w.Code, want, w.Body.String())
	}
	var out billingEventsResponse
	if want != http.StatusOK {
		return out
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("authenticated event response lost no-store policy")
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 9 {
		t.Fatalf("unexpected event response fields: %s", envelope.Data)
	}
	for _, key := range []string{"as_of", "timezone", "period", "from", "to", "page", "page_size", "total", "items"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing event response field %q", key)
		}
	}
	if err := json.Unmarshal(envelope.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.AsOf.Before(before.Truncate(time.Microsecond)) || out.AsOf.After(after) || out.Timezone != "Asia/Shanghai" || out.Items == nil {
		t.Fatalf("invalid snapshot metadata or null item array: %+v", out)
	}
	from, to := billingEventsCalendarWindow(out.AsOf, out.Period)
	if !out.From.Equal(from) || !out.To.Equal(to) {
		t.Fatalf("window does not match snapshot: %+v", out)
	}
	for _, item := range out.Items {
		keys := []string{"id", "event_type", "occurred_at", "instance_id", "instance_name", "grant_source", "amount_pt", "available_before_pt", "available_after_pt", "hourly_before_pt", "hourly_after_pt"}
		if len(item) != len(keys) {
			t.Fatalf("private or missing event fields: %#v", item)
		}
		for _, key := range keys {
			if _, ok := item[key]; !ok {
				t.Fatalf("missing event field %q: %#v", key, item)
			}
		}
		if _, ok := item["id"].(string); !ok {
			t.Fatalf("event ID is not a string: %#v", item)
		}
		at, err := time.Parse(time.RFC3339Nano, item["occurred_at"].(string))
		if err != nil || at.Before(out.From) || !at.Before(out.To) || at.After(out.AsOf) {
			t.Fatalf("event outside snapshot/window: %#v %+v", item, out)
		}
	}
	return out
}

func billingEventsCalendarWindow(at time.Time, period string) (time.Time, time.Time) {
	loc := time.FixedZone("Asia/Shanghai", 8*60*60)
	at = at.In(loc)
	from := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, loc)
	switch period {
	case "today":
		return from, from.AddDate(0, 0, 1)
	case "week":
		for from.Weekday() != time.Monday {
			from = from.AddDate(0, 0, -1)
		}
		return from, from.AddDate(0, 0, 7)
	case "month":
		from = time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, loc)
		return from, from.AddDate(0, 1, 0)
	default:
		from = time.Date(at.Year(), 1, 1, 0, 0, 0, 0, loc)
		return from, from.AddDate(1, 0, 0)
	}
}

func (f *billingEventsMySQLFixture) event(t *testing.T, id uint64, key string, user uint64, account any, at time.Time, extra map[string]any) {
	t.Helper()
	item := map[string]any{"id": id, "user_id": user, "account_id": account, "business_key": key, "event_type": "credit.grant", "occurred_at": at, "published": true}
	for key, value := range extra {
		item[key] = value
	}
	if err := f.db.Table("billing_events").Create(&item).Error; err != nil {
		t.Fatal(err)
	}
}

func (f *billingEventsMySQLFixture) financialState(t *testing.T) map[string][]map[string]any {
	t.Helper()
	state := make(map[string][]map[string]any)
	for _, table := range []string{"credit_accounts", "credit_buckets", "credit_ledger", "credit_reservations", "usage_sessions", "usage_charges", "tasks", "billing_events", "user_sessions"} {
		var rows []map[string]any
		if err := f.db.Table(table).Order("id").Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		state[table] = rows
	}
	return state
}

func TestMySQLBillingEventsRealSessionSelfAuthorizationAndReadOnly(t *testing.T) {
	f := newBillingEventsMySQLFixture(t)
	var initial int64
	if err := f.db.Table("billing_events").Count(&initial).Error; err != nil {
		t.Fatal(err)
	}
	if initial != 0 {
		t.Fatal("historical financial rows were backfilled into the event feed")
	}
	at := time.Now().Truncate(time.Microsecond)
	f.event(t, 1, "same-business-key", 2, 1, at, nil)
	f.event(t, 2, "same-business-key", 1, 2, at, nil)
	f.event(t, 3, "hidden", 2, 1, at, map[string]any{"published": false})
	f.event(t, 4, "no-wallet", 3, nil, at, nil)
	before := f.financialState(t)
	for _, token := range []string{"", "malformed", base64.RawURLEncoding.EncodeToString(make([]byte, 32))} {
		f.call(t, "?period=year", token, 401)
	}
	for _, tc := range []struct {
		user uint64
		id   string
	}{{2, "1"}, {1, "2"}, {3, "4"}} {
		out := f.call(t, "?period=year", f.tokens[tc.user], 200)
		if out.Total != 1 || len(out.Items) != 1 || out.Items[0]["id"] != tc.id || out.Page != 1 || out.PageSize != 20 {
			t.Fatalf("user %d saw foreign/unpublished events or lost no-wallet event: %+v", tc.user, out)
		}
	}
	for _, query := range []string{
		"", "?period=", "?period=all", "?period=year&period=year", "?period=year&page=1&page=1",
		"?period=year&page_size=20&page_size=20", "?period=year&user_id=1", "?period=year&account_id=2",
		"?period=year&instance_id=1", "?period=year&search=x", "?period=year&event_type=credit.grant",
		"?period=year&published=true", "?period=year&from=2026-01-01", "?period=year&unknown=1",
		"?period=%ZZ", "?period=year;page=2", "?period=year&page=", "?period=year&page=0",
		"?period=year&page=-1", "?period=year&page=%2B1", "?period=year&page=1.0",
		"?period=year&page=2147483648", "?period=year&page_size=0", "?period=year&page_size=101",
	} {
		f.call(t, query, f.tokens[2], 422)
	}
	if after := f.financialState(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("event GET mutated financial state:\nbefore=%#v\nafter=%#v", before, after)
	}
	if err := f.db.Exec("UPDATE user_sessions SET revoked_at=? WHERE user_id=2", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	f.call(t, "?period=year", f.tokens[2], 401)
	if err := f.db.Exec("UPDATE users SET status='disabled' WHERE id=3").Error; err != nil {
		t.Fatal(err)
	}
	f.call(t, "?period=year", f.tokens[3], 401)
}

func TestMySQLBillingEventsStableDescendingPagingAndNullablePublicFields(t *testing.T) {
	f := newBillingEventsMySQLFixture(t)
	at := time.Now().Truncate(time.Microsecond)
	f.event(t, 9007199254740994, "older-higher-id", 2, 1, at.Add(-time.Microsecond), nil)
	f.event(t, 9007199254740992, "same-time-low-id", 2, 1, at, map[string]any{
		"instance_id": uint64(9007199254740995), "instance_name": "Deleted instance snapshot", "grant_source": "admin",
		"amount_pt": "123456789012345678901234.123456", "available_before_pt": "-0.000001", "available_after_pt": "0.000000",
		"hourly_before_pt": "0.000000", "hourly_after_pt": "1.123456",
	})
	f.event(t, 9007199254740993, "same-time-high-id", 2, 1, at, nil)
	before := f.financialState(t)
	for range 2 {
		first := f.call(t, "?period=year&page_size=1", f.tokens[2], 200)
		if first.Total != 3 || len(first.Items) != 1 || first.Items[0]["id"] != "9007199254740993" || first.Page != 1 || first.PageSize != 1 {
			t.Fatalf("first page: %+v", first)
		}
		for _, key := range []string{"instance_id", "instance_name", "grant_source", "amount_pt", "available_before_pt", "available_after_pt", "hourly_before_pt", "hourly_after_pt"} {
			if first.Items[0][key] != nil {
				t.Fatalf("SQL NULL became a fabricated value for %s: %#v", key, first.Items[0])
			}
		}
		second := f.call(t, "?period=year&page_size=1&page=2", f.tokens[2], 200)
		if second.Total != 3 || len(second.Items) != 1 || second.Items[0]["id"] != "9007199254740992" || second.Page != 2 {
			t.Fatalf("timestamp tie did not use descending ID: %+v", second)
		}
		for key, want := range map[string]any{
			"instance_id": "9007199254740995", "instance_name": "Deleted instance snapshot", "grant_source": "admin",
			"amount_pt": "123456789012345678901234.123456", "available_before_pt": "-0.000001", "available_after_pt": "0.000000",
			"hourly_before_pt": "0.000000", "hourly_after_pt": "1.123456",
		} {
			if second.Items[0][key] != want {
				t.Fatalf("public field %s=%#v want=%#v", key, second.Items[0][key], want)
			}
		}
		third := f.call(t, "?period=year&page_size=1&page=3", f.tokens[2], 200)
		if third.Total != 3 || len(third.Items) != 1 || third.Items[0]["id"] != "9007199254740994" {
			t.Fatalf("ID sorted ahead of timestamp: %+v", third)
		}
	}
	for _, query := range []string{"?period=year&page_size=1&page=4", "?period=year&page=2147483647&page_size=100"} {
		out := f.call(t, query, f.tokens[2], 200)
		if out.Total != 3 || len(out.Items) != 0 {
			t.Fatalf("out-of-range page lost total or fabricated events: %+v", out)
		}
	}
	if after := f.financialState(t); !reflect.DeepEqual(before, after) {
		t.Fatal("paging event feed changed financial rows")
	}
}

func TestMySQLBillingEventsShanghaiCalendarBoundariesAndAsOfClipping(t *testing.T) {
	f := newBillingEventsMySQLFixture(t)
	for _, period := range []string{"today", "week", "month", "year"} {
		t.Run(period, func(t *testing.T) {
			if err := f.db.Exec("DELETE FROM billing_events").Error; err != nil {
				t.Fatal(err)
			}
			at := time.Now().Truncate(time.Microsecond)
			from, to := billingEventsCalendarWindow(at, period)
			f.event(t, 1, "before-start", 2, 1, from.Add(-time.Microsecond), nil)
			f.event(t, 2, "at-start", 2, 1, from, nil)
			f.event(t, 3, "elapsed", 2, 1, at, nil)
			f.event(t, 4, "future-in-period", 2, 1, to.Add(-time.Microsecond), nil)
			f.event(t, 5, "at-end", 2, 1, to, nil)
			before := f.financialState(t)
			out := f.call(t, "?period="+period, f.tokens[2], 200)
			if out.Period != period || !out.From.Equal(from) || !out.To.Equal(to) || out.Total != 2 || len(out.Items) != 2 || out.Items[0]["id"] != "3" || out.Items[1]["id"] != "2" {
				t.Fatalf("%s boundary or as_of clipping failure: %+v", period, out)
			}
			if after := f.financialState(t); !reflect.DeepEqual(before, after) {
				t.Fatal("calendar feed read changed financial rows")
			}
		})
	}
}

func TestMySQLBillingEventsReadOnlyRepeatableReadSnapshot(t *testing.T) {
	f := newBillingEventsMySQLFixture(t)
	at := time.Now().Add(-time.Second).Truncate(time.Microsecond)
	f.event(t, 1, "snapshot-original", 2, 1, at, nil)
	before := f.financialState(t)
	interleaved := false
	const callback = "billing_events_test:interleave_after_count"
	if err := f.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "billing_events" || interleaved {
			return
		}
		interleaved = true
		// This uses the request's transaction connection, not the fixture pool.
		// A read-only contract must reject writes even if a later refactor
		// accidentally introduces a financial mutation into this read path.
		if err := tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE credit_accounts SET revision=revision+1 WHERE id=1").Error; err == nil {
			t.Fatal("event snapshot allowed a financial write")
		}
		// A separate connection commits after COUNT but before the item read.
		// Both reads must continue to observe the request's original snapshot.
		f.event(t, 2, "snapshot-concurrent", 2, 1, at, nil)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Callback().Query().Remove(callback) })
	first := f.call(t, "?period=year", f.tokens[2], 200)
	if !interleaved || first.Total != 1 || len(first.Items) != 1 || first.Items[0]["id"] != "1" {
		t.Fatalf("COUNT and item reads did not share one snapshot: %+v interleaved=%v", first, interleaved)
	}
	second := f.call(t, "?period=year", f.tokens[2], 200)
	if second.Total != 2 || len(second.Items) != 2 || second.Items[0]["id"] != "2" || second.Items[1]["id"] != "1" {
		t.Fatalf("later request did not see committed concurrent event: %+v", second)
	}
	after := f.financialState(t)
	delete(before, "billing_events")
	delete(after, "billing_events")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("event snapshot mutated financial state")
	}
}
