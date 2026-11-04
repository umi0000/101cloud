package billing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

func addAllowanceProfile(t *testing.T, f *adjustmentMySQLFixture, source, publicID uint64, config map[string]any) {
	t.Helper()
	validated, err := validatePolicy("credit", encodedPolicy(t, config))
	if err != nil {
		t.Fatal(err)
	}
	values := sourceFields("credit", validated)
	for key, value := range (row{"id": source, "code": "explicit-credit-" + sid(source), "version": "1", "status": "active", "effective_at": "2000-01-01", "created_by_user_id": 1}) {
		values[key] = value
	}
	if err = f.db.Table("credit_policy_versions").Create(&values).Error; err != nil {
		t.Fatal(err)
	}
	if err = f.db.Exec(`INSERT INTO policy_versions(id,kind,code,version,source_id,config,status,effective_at,created_by_user_id) VALUES(?,'credit',?,'1',?,?,'active','2000-01-01',1)`, publicID, "explicit-credit-"+sid(source), source, jsonValue(config)).Error; err != nil {
		t.Fatal(err)
	}
}

func explicitAccountCall(f *adjustmentMySQLFixture, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/credit-accounts", strings.NewReader(body)).WithContext(f.ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, req)
	return response
}

func TestMySQLExplicitAccountSetupRequiresSelectedAllowanceAndReplaysWithoutGrant(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	addAllowanceProfile(t, f, 2, 102, validCreditPolicy())
	for _, tc := range []struct{ name, body, key string }{
		{"missing_policy", `{"user_id":"1"}`, "missing-policy"},
		{"wrong_kind", `{"user_id":"1","policy_version_id":"100"}`, "wrong-kind"},
		{"missing_key", `{"user_id":"1","policy_version_id":"102"}`, ""},
	} {
		response := explicitAccountCall(f, tc.body, tc.key)
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s", tc.name, response.Code, response.Body.String())
		}
	}
	if f.count(t, "credit_accounts", "user_id=1") != 0 {
		t.Fatal("invalid setup created an allowance account")
	}
	body := `{"user_id":"1","policy_version_id":"102"}`
	response := explicitAccountCall(f, body, "explicit-account")
	if response.Code != http.StatusCreated || response.Header().Get("Location") == "" || response.Header().Get("ETag") == "" {
		t.Fatalf("setup: %d %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data row `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data["policy_version_id"] != "102" || envelope.Data["remaining_pt"] != "0.000000" || !reflect.DeepEqual(envelope.Data["enforcements"], []any{}) {
		t.Fatalf("setup DTO did not expose the selected public policy and empty wallet: %#v", envelope.Data)
	}
	if _, exists := envelope.Data["enforcement"]; exists {
		t.Fatal("obsolete singular enforcement survived")
	}
	if f.count(t, "credit_accounts", "user_id=1 AND credit_policy_version_id=2") != 1 || f.count(t, "credit_buckets", "1=1") != 0 || f.count(t, "credit_daily_grants", "1=1") != 0 || f.count(t, "credit_ledger", "1=1") != 0 {
		t.Fatal("account setup chose another source or granted funds")
	}
	replay := explicitAccountCall(f, body, "explicit-account")
	var replayEnvelope struct {
		Data row `json:"data"`
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayEnvelope); err != nil {
		t.Fatal(err)
	}
	if replay.Code != response.Code || !reflect.DeepEqual(replayEnvelope.Data, envelope.Data) || replay.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatalf("setup replay changed the committed response: %d %s", replay.Code, replay.Body.String())
	}
	if conflict := explicitAccountCall(f, `{"user_id":"1","policy_version_id":"1"}`, "explicit-account"); conflict.Code != http.StatusConflict {
		t.Fatalf("changed replay accepted: %d %s", conflict.Code, conflict.Body.String())
	}
	if duplicate := explicitAccountCall(f, body, "another-setup"); duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate wallet accepted: %d %s", duplicate.Code, duplicate.Body.String())
	}
}

func TestMySQLDailyAllowanceStartsAtAccountCreationAndIgnoresInstanceProfiles(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	profile := validCreditPolicy()
	profile["daily_allowance_pt"] = "2.000000"
	addAllowanceProfile(t, f, 2, 102, profile)
	otherPlan := validCreditPolicy()
	otherPlan["daily_allowance_pt"] = "99.000000"
	addAllowanceProfile(t, f, 3, 103, otherPlan)
	otherInstance := debtRow(t, f.db, "instances", 1)
	otherInstance["id"], otherInstance["incus_name"] = 2, "other-plan-instance"
	otherInstance["credit_policy_version_id"], otherInstance["policy_versions"] = 3, jsonValue(row{"credit": "103", "metrics": "100"})
	if err := f.db.Table("instances").Create(&otherInstance).Error; err != nil {
		t.Fatal(err)
	}
	cycle, err := cycleAt(profile, now())
	if err != nil {
		t.Fatal(err)
	}
	// Yesterday's scheduled grant precedes wallet creation and is not owed.
	created := cycle.AddDate(0, 0, -1).Add(time.Second)
	if err = f.db.Table("credit_accounts").Where("id=1").Updates(row{"credit_policy_version_id": 2, "created_at": created, "grant_cursor_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = dailyGrants(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
			t.Fatal(err)
		}
	}
	if f.count(t, "credit_accounts", "user_id=1") != 0 || f.count(t, "credit_accounts", "1=1") != 1 {
		t.Fatal("daily polling automatically configured an unconfigured user")
	}
	if f.count(t, "credit_daily_grants", "account_id=1 AND policy_version_id=2") != 1 || f.count(t, "credit_buckets", "source_type='daily' AND granted_pt=2") != 1 {
		t.Fatal("daily grant did not use the independent wallet allowance exactly once")
	}
	if got := stamp(debtRow(t, f.db, "credit_accounts", 1)["grant_cursor_at"]); !got.Equal(cycle) {
		t.Fatalf("cursor=%v, want first post-creation boundary %v", got, cycle)
	}
}

func TestMySQLStorageLifecycleUsesInstanceCreditNotAllowance(t *testing.T) {
	for _, tc := range []struct {
		mode                       string
		reserved, running, stopped int64
	}{{"retained", 1, 1, 1}, {"running_only", 0, 1, 0}, {"free", 0, 0, 0}} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newAdjustmentMySQLFixture(t)
			profile := validCreditPolicy()
			profile["storage_charge_mode"] = tc.mode
			addAllowanceProfile(t, f, 2, 102, profile)
			wantAllowance := "1"
			if tc.mode == "retained" {
				allowance := validCreditPolicy()
				allowance["storage_charge_mode"] = "free"
				addAllowanceProfile(t, f, 3, 103, allowance)
				if err := f.db.Exec("UPDATE credit_accounts SET credit_policy_version_id=3 WHERE id=1").Error; err != nil {
					t.Fatal(err)
				}
				wantAllowance = "3"
			}
			if err := f.db.Exec(`UPDATE instances SET credit_policy_version_id=2,policy_versions=JSON_OBJECT('credit','102','metrics','100'),disk_gib_hourly_pt=1 WHERE id=1`).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.Exec("UPDATE usage_sessions SET status='ended',ended_at=? WHERE id=2", now()).Error; err != nil {
				t.Fatal(err)
			}
			adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"100.000000"}`, "storage-funds", f.etag(t)))
			for _, step := range []struct {
				event string
				want  int64
			}{{"disk_reserved", tc.reserved}, {"running", tc.running}, {"stopped", tc.stopped}} {
				if err := f.db.Transaction(func(db *gorm.DB) error {
					instance, err := platform.Get(db, "instances", 1, false)
					if err != nil {
						return err
					}
					return InstanceUsage(f.ctx, db, instance, step.event, nil)
				}); err != nil {
					t.Fatal(err)
				}
				if got := f.count(t, "usage_sessions", "instance_id=1 AND charge_kind='storage' AND status='active'"); got != step.want {
					t.Fatalf("%s storage=%d want=%d", step.event, got, step.want)
				}
			}
			if f.count(t, "credit_accounts", "id=1 AND credit_policy_version_id="+wantAllowance) != 1 {
				t.Fatal("selecting an instance policy rebound the shared allowance")
			}
		})
	}
}

func addElasticBillingInstance(t *testing.T, f *adjustmentMySQLFixture, id, source, publicID uint64) {
	t.Helper()
	instance := debtRow(t, f.db, "instances", 1)
	instance["id"], instance["incus_name"], instance["logical_name"] = id, "explicit-instance-"+sid(id), "Explicit instance "+sid(id)
	instance["credit_policy_version_id"] = source
	instance["policy_versions"] = jsonValue(row{"credit": sid(publicID), "metrics": "100"})
	instance["instance_plan_id"], instance["product_type"], instance["plan_snapshot"] = 1, "elastic", `{}`
	instance["desired_state"], instance["observed_state"] = "running", "running"
	if id == 1 {
		delete(instance, "id")
		if err := f.db.Table("instances").Where("id=1").Updates(instance).Error; err != nil {
			t.Fatal(err)
		}
	} else if err := f.db.Table("instances").Create(&instance).Error; err != nil {
		t.Fatal(err)
	}
}

func TestMySQLMixedPlanEnforcementPreservesHistoricalMembershipAndRestoresEverySource(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	second := validCreditPolicy()
	second["shutdown_grace_seconds"], second["billing_window_seconds"], second["grace_charge_mode"] = 1200, 300, "accrue"
	addAllowanceProfile(t, f, 2, 102, second)
	allowance := validCreditPolicy()
	allowance["billing_window_seconds"], allowance["shutdown_grace_seconds"], allowance["storage_charge_mode"] = 1, 90, "free"
	addAllowanceProfile(t, f, 3, 103, allowance)
	if err := f.db.Exec(`INSERT INTO instance_plans(id,code,version,name,image_type,vcpus,memory_bytes,disk_bytes,gpu_count,pricing_version_id,credit_policy_version_id,idle_policy_version_id,init_parameters,allow_residency,status,allowed_overrides,policy_versions,gpu_policy,allowed_user_actions,gpu_user_permissions) VALUES(1,'billing-plan','1','Billing plan','container',1,1073741824,1073741824,0,1,1,1,JSON_OBJECT(),FALSE,'active',JSON_OBJECT(),JSON_OBJECT('credit','1','metrics','100'),JSON_OBJECT(),JSON_ARRAY(),JSON_OBJECT())`).Error; err != nil {
		t.Fatal(err)
	}
	addElasticBillingInstance(t, f, 1, 1, 1)
	addElasticBillingInstance(t, f, 2, 2, 102)
	addElasticBillingInstance(t, f, 3, 2, 102)
	f.addDebt(t, []string{"1.000000"})
	at := now()
	deadline := at.Add(100 * time.Second)
	legacyPolicy := validCreditPolicy()
	legacyPolicy["source_id"], legacyPolicy["policy_version_id"] = 1, 1
	legacy, err := insert(f.db, "credit_enforcements", row{"account_id": 1, "user_id": 2, "policy_version_id": 1, "status": "grace", "exhausted_at": at.Add(-time.Minute), "shutdown_deadline": deadline, "policy_snapshot": jsonValue(legacyPolicy)})
	if err != nil {
		t.Fatal(err)
	}
	member, err := insert(f.db, "credit_enforcement_instances", row{"enforcement_id": legacy["id"], "instance_id": 3, "status": "pending"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = f.db.Transaction(func(db *gorm.DB) error {
			ac, err := platform.Get(db, "credit_accounts", 1, true)
			if err != nil {
				return err
			}
			return detectExhaustion(db, ac, at)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(legacy, debtRow(t, f.db, "credit_enforcements", platform.Uint(legacy["id"]))) || !reflect.DeepEqual(member, debtRow(t, f.db, "credit_enforcement_instances", platform.Uint(member["id"]))) {
		t.Fatal("historical event snapshot/deadline or existing member changed")
	}
	if f.count(t, "credit_enforcements", "status='grace'") != 2 || f.count(t, "credit_enforcement_instances", "1=1") != 3 || f.count(t, "credit_enforcement_instances", "instance_id=3") != 1 {
		t.Fatal("mixed sources merged events or double-enrolled a historical active member")
	}
	newEvent, err := find(f.db, "credit_enforcements", "policy_version_id=2")
	if err != nil {
		t.Fatal(err)
	}
	if !stamp(newEvent["shutdown_deadline"]).Equal(at.Add(1200 * time.Second)) {
		t.Fatal("instance shutdown deadline followed wallet allowance instead of frozen credit source")
	}
	dto, err := accountDTO(f.db, debtRow(t, f.db, "credit_accounts", 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(dto["enforcements"].([]row)) != 2 {
		t.Fatal("account DTO omitted a concurrent enforcement source")
	}
	if response := f.call(http.MethodPatch, `{"policy_version_id":"103"}`, "", f.etag(t)); response.Code != http.StatusOK {
		t.Fatalf("independent allowance change blocked by live instances/events: %d %s", response.Code, response.Body.String())
	}
	if f.count(t, "credit_accounts", "id=1 AND credit_policy_version_id=3") != 1 || f.count(t, "instances", "credit_policy_version_id=2") != 2 || !reflect.DeepEqual(legacy, debtRow(t, f.db, "credit_enforcements", platform.Uint(legacy["id"]))) {
		t.Fatal("allowance reassignment mutated frozen instance economics or historical enforcement")
	}
	// Settle the two live sources at the same time. Only source one's event waives grace.
	if err = f.db.Exec(`UPDATE nodes SET health='online' WHERE id=1`).Error; err != nil {
		t.Fatal(err)
	}
	if err = f.db.Table("usage_sessions").Where("id=2").Updates(row{"config_snapshot": `{"vcpus":1}`, "last_settled_at": at, "metering_state": "running"}).Error; err != nil {
		t.Fatal(err)
	}
	if err = f.db.Transaction(func(db *gorm.DB) error {
		ac := debtRow(t, db, "credit_accounts", 1)
		p, err := policyFor(db, 2)
		if err != nil {
			return err
		}
		if err = beginSession(db, ac, p, debtRow(t, db, "instances", 2), "base_compute", nil, "running", at); err != nil {
			return err
		}
		live, err := rows(db, "usage_sessions", "status='active'")
		if err != nil {
			return err
		}
		for _, session := range live {
			p, err := sessionCreditPolicy(db, session)
			if err != nil {
				return err
			}
			if err = settleSession(db, ac, p, session, at.Add(time.Minute)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "usage_charges c JOIN usage_sessions s ON s.id=c.session_id", "s.instance_id=1 AND c.from_at='"+at.Format("2006-01-02 15:04:05.000000")+"' AND c.expected_pt=0") != 1 {
		t.Fatal("waived instance grace was charged")
	}
	if f.count(t, "usage_charges c JOIN usage_sessions s ON s.id=c.session_id", "s.instance_id=2 AND c.expected_pt>0") != 1 {
		t.Fatal("charged instance grace followed another source's waiver")
	}
	adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"1.066667"}`, "partial-restore-mixed", f.etag(t)))
	if err = settlePending(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "credit_enforcements", "status IN ('grace','stopping','unknown')") != 2 || f.count(t, "notifications", "type='credit_restored'") != 0 {
		t.Fatal("restoration used the allowance window instead of each live instance's frozen window")
	}
	adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"100.000000"}`, "restore-mixed", f.etag(t)))
	if err = enforceAll(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "credit_enforcements", "status='cancelled'") != 2 || f.count(t, "notifications", "type='credit_restored'") != 2 {
		t.Fatal("shared wallet restoration did not cancel and notify every active source")
	}
	if !stamp(debtRow(t, f.db, "credit_enforcements", platform.Uint(legacy["id"]))["shutdown_deadline"]).Equal(deadline) {
		t.Fatal("restoration rewrote a historical deadline")
	}
}

func TestMySQLDailyPollingNeedsNoPolicyUntilAnAccountIsConfigured(t *testing.T) {
	ctx, db := openBillingMySQL(t, "ci_unconfigured_allowance_")
	if err := dailyGrants(ctx, platform.New(db, platform.Config{})); err != nil {
		t.Fatalf("empty installation required an arbitrary allowance policy: %v", err)
	}
}

func TestMySQLUnconfiguredExemptLifecycleDoesNotCreateFinancialState(t *testing.T) {
	ctx, db := openBillingMySQL(t, "ci_exempt_no_wallet_")
	instance := row{"billing_mode": "admin_exempt", "user_id": uint64(1), "id": uint64(1)}
	for _, event := range []string{"disk_reserved", "running", "stopped", "resize_stopped", "deleted", "resize", "resize_reserved", "resize_running_reserved", "gpu_reserved", "gpu_attached", "gpu_released"} {
		if err := InstanceUsage(ctx, db, instance, event, nil); err != nil {
			t.Fatalf("%s unexpectedly requires an allowance: %v", event, err)
		}
	}
	for _, table := range []string{"credit_accounts", "usage_sessions", "credit_buckets", "credit_ledger", "usage_charges"} {
		var count int64
		if err := db.Table(table).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("unconfigured exempt lifecycle wrote %s: count=%d error=%v", table, count, err)
		}
	}
}

func TestMySQLConfiguredExemptSessionStillFinalizesWithoutCharge(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	if err := f.db.Exec("UPDATE instances SET billing_mode='admin_exempt' WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec("UPDATE usage_sessions SET billing_mode='admin_exempt' WHERE id=2").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transaction(func(db *gorm.DB) error {
		instance, err := platform.Get(db, "instances", 1, false)
		if err != nil {
			return err
		}
		return InstanceUsage(f.ctx, db, instance, "stopped", nil)
	}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "usage_sessions", "id=2 AND status='ended' AND ended_at IS NOT NULL") != 1 || f.count(t, "usage_charges", "expected_pt>0 OR settled_pt>0") != 0 || f.count(t, "credit_ledger", "1=1") != 0 {
		t.Fatal("historical configured exempt session failed to close as zero-charge history")
	}
}
