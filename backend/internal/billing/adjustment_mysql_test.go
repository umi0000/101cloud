package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type adjustmentMySQLFixture struct {
	ctx    context.Context
	db     *gorm.DB
	router *gin.Engine
}

func newAdjustmentMySQLFixture(t *testing.T) *adjustmentMySQLFixture {
	t.Helper()
	ctx, db := openBillingMySQL(t, "ci_adjustment_")
	// Keep production constraints, indexes, and mutation transactions intact. The
	// instance is custom so no unrelated elastic-plan admission is required.
	for _, statement := range []string{
		`INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'adjustment-admin','unused-test-hash','admin','active'),(2,'adjustment-owner','unused-test-hash','user','active')`,
		`INSERT INTO pricing_versions(id,code,version,cpu_hourly_pt,memory_gib_hourly_pt,disk_gib_hourly_pt,status,effective_at,created_by_user_id) VALUES(1,'adjustment-price','1',1,0,0,'active','2000-01-01',1)`,
		`INSERT INTO credit_policy_versions(id,code,version,daily_allowance_pt,bonus_cap_pt,timezone,reset_time,billing_window_seconds,status,effective_at,created_by_user_id,daily_validity_seconds,shutdown_grace_seconds,grace_charge_mode,restore_action,storage_charge_mode,offline_charge_mode) VALUES(1,'adjustment-credit','1',0,0,'Asia/Shanghai','06:30:00',60,'active','2000-01-01',1,86400,600,'waived','cancel_pending_stop','running_only','defer')`,
		`INSERT INTO idle_policy_versions(id,code,version,sample_interval_seconds,evaluation_window_seconds,consecutive_idle_windows,cpu_utilization_threshold_pct,gpu_utilization_threshold_pct,gpu_memory_threshold_pct,network_bytes_threshold,action,status,effective_at,created_by_user_id) VALUES(1,'adjustment-idle','1',10,60,2,5,5,5,0,'release_gpu','active','2000-01-01',1)`,
		`INSERT INTO credit_accounts(id,user_id,credit_policy_version_id,status) VALUES(1,2,1,'active')`,
		`INSERT INTO policy_versions(id,kind,code,version,config,status,effective_at,created_by_user_id) VALUES(100,'metrics','adjustment-metrics','1',JSON_OBJECT('raw_retention_days',7,'rollup_retention_days',30,'bucket_seconds',60,'collection_interval_seconds',5),'active','2000-01-01',1)`,
		`INSERT INTO nodes(id,name,agent_endpoint,token_ciphertext,key_version,server_cert_sha256,default_storage_pool,default_project_name,allocatable_vcpus,allocatable_memory_bytes,allocatable_disk_bytes,resident_vcpu_budget,resident_memory_budget_bytes,resident_disk_budget_bytes,status,metrics_policy_id) VALUES(1,'adjustment-node','https://adjustment.invalid:8443','unused-cipher',1,UNHEX(SHA2('adjustment-cert',256)),'default','default',4,1073741824,1073741824,4,1073741824,1073741824,'active',100)`,
		`INSERT INTO images(id,code,version,name,source_type,fingerprint,architecture,image_type,gpu_ready,os_name,os_release,properties,status,created_by_user_id) VALUES(1,'adjustment-image','1','Adjustment image','local',UNHEX(SHA2('adjustment-image',256)),'x86_64','container',FALSE,'Linux','test',JSON_OBJECT(),'active',1)`,
		`INSERT INTO instances(id,user_id,node_id,image_id,instance_plan_id,logical_name,project_name,incus_name,image_type,architecture,image_fingerprint,vcpus,memory_bytes,disk_bytes,storage_pool,pricing_version_id,cpu_hourly_pt,memory_gib_hourly_pt,disk_gib_hourly_pt,credit_policy_version_id,billing_mode,desired_state,observed_state,provisioning_state,auto_release_gpu,auto_rebind_gpu,idle_policy_version_id,max_gpu_hourly_pt,init_parameters,incus_config_snapshot,product_type,allowed_user_actions,gpu_user_permissions,policy_versions,gpu_policy) VALUES(1,2,1,1,NULL,'Adjustment instance','default','adjustment-instance','container','x86_64',UNHEX(SHA2('adjustment-image',256)),1,1073741824,1073741824,'default',1,1,0,0,1,'credit','stopped','stopped','ready',FALSE,FALSE,1,0,JSON_OBJECT(),JSON_OBJECT(),'custom',JSON_ARRAY(),JSON_OBJECT(),JSON_OBJECT(),JSON_OBJECT())`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	policy := validCreditPolicy()
	policy["daily_allowance_pt"] = "0.000000"
	if err := db.Exec(`INSERT INTO policy_versions(id,kind,code,version,source_id,config,status,effective_at,created_by_user_id) VALUES(1,'credit','adjustment-credit','1',1,?,'active','2000-01-01',1)`, jsonValue(policy)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE instances SET policy_versions=JSON_OBJECT('credit','1','metrics','100') WHERE id=1`).Error; err != nil {
		t.Fatal(err)
	}
	at := now().Add(-72 * time.Hour)
	for id := uint64(1); id <= 2; id++ {
		session := row{
			"id": id, "account_id": 1, "user_id": 2, "instance_id": 1, "node_id": 1,
			"charge_kind": "base_compute", "pricing_version_id": 1, "billing_mode": "credit",
			"config_snapshot": `{}`, "cpu_hourly_pt": "1.000000", "memory_gib_hourly_pt": "0.000000",
			"disk_gib_hourly_pt": "0.000000", "gpu_hourly_pt": "0.000000", "status": "ended",
			"started_at": at, "last_settled_at": at.Add(48 * time.Hour), "ended_at": at.Add(48 * time.Hour),
		}
		if id == 2 {
			session["status"] = "active"
			session["last_settled_at"] = now()
			session["ended_at"] = nil
		}
		if err := db.Table("usage_sessions").Create(&session).Error; err != nil {
			t.Fatal(err)
		}
	}
	a := platform.New(db, platform.Config{})
	router := gin.New()
	router.Use(platform.RequestMiddleware())
	admin := router.Group("/api/v1/admin", func(c *gin.Context) {
		c.Set("user", platform.User{ID: 1, Role: "admin"})
		c.Next()
	}, platform.Admin(), a.Mutations())
	Register(a, router.Group("/api/v1/user"), admin)
	return &adjustmentMySQLFixture{ctx: ctx, db: db, router: router}
}

func (f *adjustmentMySQLFixture) etag(t *testing.T) string {
	t.Helper()
	return `"rev-` + sid(debtRow(t, f.db, "credit_accounts", 1)["revision"]) + `"`
}

func (f *adjustmentMySQLFixture) call(method, body, key, etag string) *httptest.ResponseRecorder {
	path := "/api/v1/admin/credit-accounts/1"
	if method == http.MethodPost {
		path += "/adjustments"
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(f.ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("If-Match", etag)
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, req)
	return response
}

func adjustmentResponse(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("adjustment status=%d: %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode adjustment: %v: %s", err, response.Body.String())
	}
	fields := []string{"account_id", "amount_pt", "bucket_id", "expires_at", "settled_debt_pt", "settlement_pending", "outstanding_debt_pt", "remaining_pt", "reserved_pt", "available_pt", "revision"}
	if len(envelope.Data) != len(fields) {
		t.Fatalf("adjustment must contain exactly the fixed result fields: %#v", envelope.Data)
	}
	for _, key := range fields {
		value, present := envelope.Data[key]
		if !present {
			t.Fatalf("missing result field %q: %#v", key, envelope.Data)
		}
		if key == "settlement_pending" {
			if _, ok := value.(bool); !ok {
				t.Fatalf("settlement_pending must be boolean: %#v", value)
			}
			continue
		}
		if value == nil && (key == "bucket_id" || key == "expires_at") {
			continue
		}
		text, ok := value.(string)
		if !ok {
			t.Fatalf("result %s must be a decimal/ID/time string, got %#v", key, value)
		}
		if strings.HasSuffix(key, "_pt") {
			value, err := decimal.NewFromString(text)
			if err != nil || value.StringFixed(6) != text {
				t.Fatalf("result %s is not an exact six-decimal string: %q", key, text)
			}
		}
	}
	if got, want := response.Header().Get("ETag"), `"rev-`+envelope.Data["revision"].(string)+`"`; got != want {
		t.Fatalf("adjustment ETag=%q, want committed revision %q", got, want)
	}
	return envelope.Data
}

func assertAdjustmentFields(t *testing.T, result map[string]any, want map[string]any) {
	t.Helper()
	for key, value := range want {
		if result[key] != value {
			t.Fatalf("result %s=%#v, want %#v; result=%#v", key, result[key], value, result)
		}
	}
}

func (f *adjustmentMySQLFixture) assertFresh(t *testing.T, result map[string]any) {
	t.Helper()
	// These reads run outside the mutation transaction, proving the returned
	// balance and account revision describe committed state, not the grant row.
	account := debtRow(t, f.db, "credit_accounts", 1)
	remaining, reserved, err := balances(f.db, 1)
	if err != nil {
		t.Fatal(err)
	}
	var debt string
	if err := f.db.Raw(`SELECT CAST(COALESCE(SUM(expected_pt-settled_pt),0) AS DECIMAL(20,6)) FROM usage_charges WHERE account_id=1 AND status='insufficient'`).Scan(&debt).Error; err != nil {
		t.Fatal(err)
	}
	assertAdjustmentFields(t, result, map[string]any{
		"account_id": "1", "revision": sid(account["revision"]),
		"remaining_pt": money(remaining.Sub(dec(debt))), "reserved_pt": money(reserved), "available_pt": money(remaining.Sub(reserved).Sub(dec(debt))),
	})
	if result["outstanding_debt_pt"] != debt {
		t.Fatalf("reported debt=%v, committed debt=%s", result["outstanding_debt_pt"], debt)
	}
	response := f.call(http.MethodGet, "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("read committed account: %d %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"remaining_pt", "reserved_pt", "available_pt", "outstanding_debt_pt", "settlement_pending", "revision"} {
		if envelope.Data[key] != result[key] {
			t.Fatalf("fresh GET %s=%v, adjustment=%v", key, envelope.Data[key], result[key])
		}
	}
}

func (f *adjustmentMySQLFixture) addDebt(t *testing.T, amounts []string) {
	t.Helper()
	at := now().Add(-48 * time.Hour)
	charges := make([]map[string]any, 0, len(amounts))
	for index, amount := range amounts {
		from := at.Add(time.Duration(index) * time.Second)
		charges = append(charges, map[string]any{
			"id": index + 1, "session_id": 1, "account_id": 1, "from_at": from, "to_at": from.Add(time.Second),
			"expected_pt": amount, "settled_pt": "0.000000", "status": "insufficient", "calculation_snapshot": `{}`,
			"revision": 7, "updated_at": time.Date(2000, 1, 1, 0, 0, 0, 0, zone),
		})
	}
	if len(charges) > 0 {
		if err := f.db.Table("usage_charges").CreateInBatches(charges, 250).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func (f *adjustmentMySQLFixture) count(t *testing.T, table, where string) int64 {
	t.Helper()
	var count int64
	if err := f.db.Table(table).Where(where).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func TestMySQLCreditAdjustmentPositiveGrantResults(t *testing.T) {
	expiry := now().Add(365 * 24 * time.Hour).Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name        string
		amount      string
		expiryJSON  string
		debt        []string
		existing    string
		settled     string
		outstanding string
		remaining   string
	}{
		{name: "omitted_expiry", amount: "1.000001", settled: "0.000000", outstanding: "0.000000", remaining: "1.000001"},
		{name: "null_expiry", amount: "0.000001", expiryJSON: `,"expires_at":null`, settled: "0.000000", outstanding: "0.000000", remaining: "0.000001"},
		{name: "expiry_and_large_exact_decimal", amount: "12345678901234.123456", expiryJSON: `,"expires_at":"` + expiry + `"`, settled: "0.000000", outstanding: "0.000000", remaining: "12345678901234.123456"},
		{name: "old_debt_consumes_grant", amount: "1.000001", debt: []string{"3.000001"}, settled: "1.000001", outstanding: "2.000000", remaining: "-2.000000"},
		{name: "expiring_grant_exactly_pays_debt", amount: "2.000002", expiryJSON: `,"expires_at":"` + expiry + `"`, debt: []string{"2.000002"}, settled: "2.000002", outstanding: "0.000000", remaining: "0.000000"},
		{name: "excess_after_historical_debt", amount: "3.000004", debt: []string{"0.000001", "2.000002"}, settled: "2.000003", outstanding: "0.000000", remaining: "1.000001"},
		{name: "repayment_includes_existing_bucket", amount: "0.000003", existing: "0.000002", debt: []string{"0.000006"}, settled: "0.000005", outstanding: "0.000001", remaining: "-0.000001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdjustmentMySQLFixture(t)
			f.addDebt(t, tc.debt)
			if tc.existing != "" {
				if err := f.db.Transaction(func(db *gorm.DB) error {
					account, err := platform.Get(db, "credit_accounts", 1, true)
					if err != nil {
						return err
					}
					_, err = grant(db, account, "admin", "existing-before-adjustment", dec(tc.existing), now().Add(-time.Hour), nil, 1)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := f.etag(t)
			result := adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"`+tc.amount+`"`+tc.expiryJSON+`}`, "positive", before))
			gross := decimal.Zero
			for _, debt := range tc.debt {
				gross = gross.Add(dec(debt))
			}
			assertAdjustmentFields(t, result, map[string]any{
				"account_id": "1", "amount_pt": tc.amount, "settled_debt_pt": "0.000000", "settlement_pending": len(tc.debt) > 0,
				"outstanding_debt_pt": money(gross), "remaining_pt": tc.remaining,
				"reserved_pt": "0.000000", "available_pt": tc.remaining,
			})
			bucketID, ok := result["bucket_id"].(string)
			if !ok || platform.Uint(bucketID) == 0 {
				t.Fatalf("positive adjustment missing bucket ID: %#v", result)
			}
			bucket := debtRow(t, f.db, "credit_buckets", platform.Uint(bucketID))
			if money(dec(bucket["granted_pt"])) != tc.amount || money(dec(bucket["remaining_pt"])) != tc.amount {
				t.Fatalf("committed bucket does not match adjustment: %#v", bucket)
			}
			if strings.Contains(tc.expiryJSON, `"expires_at":"`) {
				text, ok := result["expires_at"].(string)
				if !ok {
					t.Fatalf("missing expiry: %#v", result)
				}
				got, err := time.Parse(time.RFC3339Nano, text)
				want, parseErr := time.Parse(time.RFC3339Nano, expiry)
				if err != nil || parseErr != nil || !got.Equal(want) || !stamp(bucket["expires_at"]).Equal(want) {
					t.Fatalf("expiry response=%q bucket=%v want=%s", text, bucket["expires_at"], expiry)
				}
			} else if result["expires_at"] != nil || bucket["expires_at"] != nil {
				t.Fatalf("non-expiring adjustment acquired expiry: result=%#v bucket=%#v", result, bucket)
			}
			f.assertFresh(t, result)
			if f.etag(t) == before {
				t.Fatal("committed adjustment did not advance account revision")
			}
			var ledger string
			if f.count(t, "credit_ledger", "entry_type='settle'") != 0 {
				t.Fatal("HTTP grant synchronously repaid historical charges")
			}
			if err := settlePending(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Raw(`SELECT CAST(COALESCE(-SUM(amount_delta_pt),0) AS DECIMAL(20,6)) FROM credit_ledger WHERE account_id=1 AND entry_type='settle'`).Scan(&ledger).Error; err != nil || ledger != tc.settled {
				t.Fatalf("debt repayment ledger=%s want=%s err=%v", ledger, tc.settled, err)
			}
		})
	}
}

func TestMySQLCreditAdjustmentDebtBalancesProgressWithoutRepinning(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	f.addDebt(t, []string{"3.000003"})
	instanceBefore := debtRow(t, f.db, "instances", 1)
	account := debtRow(t, f.db, "credit_accounts", 1)
	initial, err := accountDTO(f.db, account)
	if err != nil {
		t.Fatal(err)
	}
	assertAdjustmentFields(t, initial, map[string]any{
		"remaining_pt": "-3.000003", "available_pt": "-3.000003", "outstanding_debt_pt": "3.000003", "reserved_pt": "0.000000",
	})
	for _, tc := range []struct {
		key         string
		amount      string
		remaining   string
		outstanding string
	}{
		{key: "partial-one", amount: "1.000001", remaining: "-2.000002", outstanding: "3.000003"},
		{key: "partial-two", amount: "1.000001", remaining: "-1.000001", outstanding: "3.000003"},
		{key: "full-plus-credit", amount: "2.000002", remaining: "1.000001", outstanding: "3.000003"},
	} {
		result := adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"`+tc.amount+`"}`, tc.key, f.etag(t)))
		assertAdjustmentFields(t, result, map[string]any{
			"remaining_pt": tc.remaining, "available_pt": tc.remaining, "outstanding_debt_pt": tc.outstanding,
		})
		f.assertFresh(t, result)
	}
	instanceAfter := debtRow(t, f.db, "instances", 1)
	if !reflect.DeepEqual(instanceBefore, instanceAfter) {
		t.Fatal("balance display and repayment changed an unrelated instance or its policy pins")
	}
}

func TestMySQLCreditAdjustmentDebtBalancesSubtractReservationOnce(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	first := adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"1.000001"}`, "reserved-credit", f.etag(t)))
	if err := f.db.Transaction(func(db *gorm.DB) error {
		if _, err := platform.Get(db, "credit_accounts", 1, true); err != nil {
			return err
		}
		bucket, err := platform.Get(db, "credit_buckets", platform.Uint(first["bucket_id"]), true)
		if err != nil {
			return err
		}
		at := now()
		reservation, err := insert(db, "credit_reservations", row{
			"reservation_key": "debt-display-held", "account_id": 1, "session_id": 2, "bucket_id": bucket["id"],
			"window_from": at, "window_to": at.Add(time.Hour), "reserved_at": at, "expires_at": at.Add(time.Hour),
			"reserved_pt": "1.000001", "settled_pt": "0.000000", "released_pt": "0.000000", "status": "held",
		})
		if err != nil {
			return err
		}
		return mutateBucket(db, bucket, "debt-display-held", "reserve", decimal.Zero, dec("1.000001"), row{"reservation_id": reservation["id"]})
	}); err != nil {
		t.Fatal(err)
	}
	f.addDebt(t, []string{"3.000003"})
	result := adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"1.000001"}`, "repay-with-held-credit", f.etag(t)))
	assertAdjustmentFields(t, result, map[string]any{
		"settled_debt_pt": "0.000000", "outstanding_debt_pt": "3.000003", "settlement_pending": true,
		"remaining_pt": "-1.000001", "reserved_pt": "1.000001", "available_pt": "-2.000002",
	})
	f.assertFresh(t, result)
	bucket := debtRow(t, f.db, "credit_buckets", platform.Uint(first["bucket_id"]))
	reservation := debtRow(t, f.db, "credit_reservations", 1)
	if money(dec(bucket["remaining_pt"])) != "1.000001" || money(dec(bucket["reserved_pt"])) != "1.000001" || platform.Text(reservation["status"]) != "held" || money(dec(reservation["settled_pt"])) != "0.000000" || money(dec(reservation["released_pt"])) != "0.000000" {
		t.Fatalf("public debt display changed reserved funds: bucket=%#v reservation=%#v", bucket, reservation)
	}
}

func TestMySQLCreditAdjustmentNegativePreservesReservationsAndRollsBack(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	expiry := now().Add(24 * time.Hour).Format(time.RFC3339Nano)
	first := adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"2.000002","expires_at":"`+expiry+`"}`, "first", f.etag(t)))
	adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"1.000001"}`, "second", f.etag(t)))
	if err := f.db.Transaction(func(db *gorm.DB) error {
		if _, err := platform.Get(db, "credit_accounts", 1, true); err != nil {
			return err
		}
		bucket, err := platform.Get(db, "credit_buckets", platform.Uint(first["bucket_id"]), true)
		if err != nil {
			return err
		}
		at := now()
		reservation, err := insert(db, "credit_reservations", row{
			"reservation_key": "adjustment-held", "account_id": 1, "session_id": 2, "bucket_id": bucket["id"],
			"window_from": at, "window_to": at.Add(time.Hour), "reserved_at": at, "expires_at": at.Add(time.Hour),
			"reserved_pt": "1.000001", "settled_pt": "0.000000", "released_pt": "0.000000", "status": "held",
		})
		if err != nil {
			return err
		}
		return mutateBucket(db, bucket, "adjustment-held", "reserve", decimal.Zero, dec("1.000001"), row{"reservation_id": reservation["id"]})
	}); err != nil {
		t.Fatal(err)
	}
	result := adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"-1.500001"}`, "negative", f.etag(t)))
	assertAdjustmentFields(t, result, map[string]any{
		"amount_pt": "-1.500001", "bucket_id": nil, "expires_at": nil,
		"settled_debt_pt": "0.000000", "outstanding_debt_pt": "0.000000",
		"remaining_pt": "1.500002", "reserved_pt": "1.000001", "available_pt": "0.500001",
	})
	f.assertFresh(t, result)
	reservation := debtRow(t, f.db, "credit_reservations", 1)
	if platform.Text(reservation["status"]) != "held" || money(dec(reservation["settled_pt"])) != "0.000000" || money(dec(reservation["released_pt"])) != "0.000000" {
		t.Fatalf("negative adjustment consumed another session's reservation: %#v", reservation)
	}
	beforeRevision := f.etag(t)
	beforeLedger := f.count(t, "credit_ledger", "account_id=1")
	beforeRequests := f.count(t, "backend_requests", "1=1")
	response := f.call(http.MethodPost, `{"amount_pt":"-0.500002"}`, "overdraw", beforeRevision)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"credit_exhausted"`) {
		t.Fatalf("overdraw status=%d: %s", response.Code, response.Body.String())
	}
	if f.etag(t) != beforeRevision || f.count(t, "credit_ledger", "account_id=1") != beforeLedger || f.count(t, "backend_requests", "1=1") != beforeRequests {
		t.Fatal("rejected adjustment committed partial debits, revision, or idempotency record")
	}
	f.assertFresh(t, result)
}

func TestMySQLCreditAdjustmentReplayReturnsOriginalCommittedResult(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	f.addDebt(t, []string{"0.000001"})
	originalETag := f.etag(t)
	body := `{"amount_pt":"0.123457","expires_at":null}`
	original := adjustmentResponse(t, f.call(http.MethodPost, body, "replay", originalETag))
	assertAdjustmentFields(t, original, map[string]any{"settled_debt_pt": "0.000000", "available_pt": "0.123456", "settlement_pending": true})
	f.assertFresh(t, original)
	current := adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"0.000001"}`, "later", f.etag(t)))
	f.assertFresh(t, current)
	beforeRevision := f.etag(t)
	beforeLedger := f.count(t, "credit_ledger", "account_id=1")
	for range 2 {
		replayed := adjustmentResponse(t, f.call(http.MethodPost, body, "replay", originalETag))
		if !reflect.DeepEqual(replayed, original) {
			t.Fatalf("replay must return original committed snapshot: original=%#v replay=%#v", original, replayed)
		}
	}
	if f.etag(t) != beforeRevision || f.count(t, "credit_buckets", "account_id=1") != 2 || f.count(t, "credit_ledger", "account_id=1") != beforeLedger || f.count(t, "backend_requests", "1=1") != 2 {
		t.Fatal("same-key replay duplicated a grant, debt payment, ledger entry, or revision")
	}
	conflict := f.call(http.MethodPost, `{"amount_pt":"0.123458","expires_at":null}`, "replay", originalETag)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"idempotency_conflict"`) {
		t.Fatalf("same-key different amount status=%d: %s", conflict.Code, conflict.Body.String())
	}
	f.assertFresh(t, current)
}

func TestMySQLCreditAdjustmentLargeDebtBacklogIsDurableAndChunked(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	const total = 6001
	amounts := make([]string, total)
	for index := range amounts {
		amounts[index] = "0.000002"
	}
	f.addDebt(t, amounts)
	firstBefore := debtRow(t, f.db, "usage_charges", 1)
	lastBefore := debtRow(t, f.db, "usage_charges", total)
	body := `{"amount_pt":"0.001027"}`
	etag := f.etag(t)
	result := adjustmentResponse(t, f.call(http.MethodPost, body, "backlog-partial", etag))
	assertAdjustmentFields(t, result, map[string]any{
		"settled_debt_pt": "0.000000", "settlement_pending": true, "outstanding_debt_pt": "0.012002", "remaining_pt": "-0.010975", "available_pt": "-0.010975",
	})
	f.assertFresh(t, result)
	assertDebtUnchanged(t, f.db, firstBefore)
	assertDebtUnchanged(t, f.db, lastBefore)
	if f.count(t, "credit_ledger", "entry_type='settle'") != 0 || f.count(t, "usage_charges", "revision<>7") != 0 {
		t.Fatal("grant must not perform any synchronous historical settlement")
	}
	// A failed worker transaction rolls back ledger, charge and retry state.
	rollback := errors.New("simulated process loss before commit")
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := platform.Get(db, "credit_accounts", 1, true)
		if err != nil {
			return err
		}
		if err = restore(db, ac, now()); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("worker rollback: %v", err)
	}
	assertDebtUnchanged(t, f.db, firstBefore)
	if f.count(t, "credit_ledger", "entry_type='settle'") != 0 {
		t.Fatal("failed chunk committed financial writes")
	}
	// A newly constructed App recovers solely from the committed database flag.
	restarted := platform.New(f.db, platform.Config{})
	if err := settlePending(f.ctx, restarted); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, "usage_charges", "status='settled'"); got != settlementChunkSize {
		t.Fatalf("one worker pass settled %d charges, want %d", got, settlementChunkSize)
	}
	assertDebtUnchanged(t, f.db, lastBefore)
	// Another real HTTP grant commits between chunks, without draining the tail.
	second := adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"0.010976"}`, "backlog-finish", f.etag(t)))
	assertAdjustmentFields(t, second, map[string]any{"settled_debt_pt": "0.000000", "settlement_pending": true, "available_pt": "0.000001"})
	if f.count(t, "usage_charges", "status='settled'") != settlementChunkSize {
		t.Fatal("second HTTP grant drained pending work")
	}
	f.assertFresh(t, second)
	for range total/settlementChunkSize + 2 {
		before := f.count(t, "usage_charges", "status='settled'")
		// Each pass may be run by an entirely new process/App.
		if err := settlePending(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
			t.Fatal(err)
		}
		after := f.count(t, "usage_charges", "status='settled'")
		if after-before > settlementChunkSize {
			t.Fatalf("unbounded chunk: %d", after-before)
		}
		if !platform.Bool(debtRow(t, f.db, "credit_accounts", 1)["settlement_pending"]) {
			break
		}
	}
	if f.count(t, "usage_charges", "status='settled' AND settled_pt=expected_pt AND settled_at IS NOT NULL AND revision=8") != total || f.count(t, "credit_ledger", "entry_type='settle'") != total+1 || f.count(t, "credit_ledger", "entry_type='settle' AND charge_id=514") != 2 {
		t.Fatal("restart recovery duplicated or skipped a per-charge settlement")
	}
	account, err := accountDTO(f.db, debtRow(t, f.db, "credit_accounts", 1))
	if err != nil {
		t.Fatal(err)
	}
	assertAdjustmentFields(t, account, map[string]any{"settlement_pending": false, "outstanding_debt_pt": "0.000000", "available_pt": "0.000001"})
	var settled string
	if err := f.db.Raw(`SELECT CAST(-SUM(amount_delta_pt) AS DECIMAL(20,6)) FROM credit_ledger WHERE entry_type='settle'`).Scan(&settled).Error; err != nil || settled != "0.012002" {
		t.Fatalf("backlog settlement total=%s err=%v", settled, err)
	}
	beforeLedger, beforeRevision := f.count(t, "credit_ledger", "1=1"), f.etag(t)
	if err := settlePending(f.ctx, restarted); err != nil {
		t.Fatal(err)
	}
	if replay := adjustmentResponse(t, f.call(http.MethodPost, body, "backlog-partial", etag)); !reflect.DeepEqual(replay, result) {
		t.Fatal("replay after restart/drain did not preserve original commit snapshot")
	}
	if f.count(t, "credit_ledger", "1=1") != beforeLedger || f.etag(t) != beforeRevision {
		t.Fatal("completed retry or replay duplicated financial work")
	}
}
