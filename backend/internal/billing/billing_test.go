package billing

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func pricedSession(cpu, memory, disk, gpu string, vcpus uint64, memoryBytes, diskBytes string) row {
	return row{"config_snapshot": jsonValue(row{"vcpus": vcpus, "memory_bytes": memoryBytes, "disk_bytes": diskBytes}), "cpu_hourly_pt": cpu, "memory_gib_hourly_pt": memory, "disk_gib_hourly_pt": disk, "gpu_hourly_pt": gpu}
}

func TestCumulativeBillingConservesPartitionedIntervals(t *testing.T) {
	cases := []struct {
		name    string
		session row
		parts   []uint64
		want    string
	}{
		{"one micro-point over many samples", pricedSession("0.000001", "0", "0", "0", 1, "0", "0"), repeatDuration(3600, 1000000), "0.000001"},
		{"mixed resources and uneven samples", pricedSession("1.250000", "0.800000", "0.200000", "0.600000", 3, "536870912", "1342177280"), []uint64{1, 999999, 17000001, 229999999, 3352000000}, "5.000000"},
		{"one byte is not rounded to zero GiB", pricedSession("0", "1073741824", "0", "0", 0, "1", "0"), []uint64{1, 999999, 3599000000}, "1.000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total := uint64(0)
			previous := decimal.Zero
			partitioned := decimal.Zero
			for _, duration := range tc.parts {
				total += duration
				cumulative := expected(tc.session, total)
				increment := cumulative.Sub(previous)
				if increment.IsNegative() {
					t.Fatal("cumulative price decreased")
				}
				partitioned = partitioned.Add(increment)
				previous = cumulative
			}
			whole := expected(tc.session, total)
			if !partitioned.Equal(whole) {
				t.Fatalf("partitioned %s != single interval %s", partitioned, whole)
			}
			if got := money(whole); got != tc.want {
				t.Fatalf("price = %s, want %s", got, tc.want)
			}
		})
	}
	// Per-sample rounding would lose this entire micro-point. This assertion makes
	// the regression exercise observable rather than only checking a telescoping sum.
	tiny := pricedSession("0.000001", "0", "0", "0", 1, "0", "0")
	if !expected(tiny, 1000000).IsZero() || money(expected(tiny, 3600000000)) != "0.000001" {
		t.Fatal("test must straddle the individual-slice rounding loss")
	}
}
func repeatDuration(count int, duration uint64) []uint64 {
	out := make([]uint64, count)
	for i := range out {
		out[i] = duration
	}
	return out
}

func TestExpectedRoundsHalfMicroPointUp(t *testing.T) {
	session := pricedSession("0.000001", "0", "0", "0", 1, "0", "0")
	for _, tc := range []struct {
		duration uint64
		want     string
	}{{0, "0.000000"}, {1799999999, "0.000000"}, {1800000000, "0.000001"}, {3600000000, "0.000001"}, {5400000000, "0.000002"}} {
		if got := money(expected(session, tc.duration)); got != tc.want {
			t.Errorf("duration %d: got %s, want %s", tc.duration, got, tc.want)
		}
	}
}

func TestMoneyPrecisionAndSignValidation(t *testing.T) {
	for _, s := range []string{"0", "0.000001", "99999999999999.999999", "12.340000"} {
		if _, e := amount(s, false); e != nil {
			t.Errorf("valid money %q rejected: %v", s, e)
		}
	}
	for _, s := range []string{"", " 1", "1 ", "1e3", ".1", "1.", "01", "+1", "NaN", "1.0000001", "100000000000000", "-0.000001"} {
		if _, e := amount(s, false); e == nil {
			t.Errorf("invalid price %q accepted", s)
		}
	}
	if v, e := amount("-12.000001", true); e != nil || money(v) != "-12.000001" {
		t.Fatalf("signed adjustment rejected or changed: %s, %v", v, e)
	}
}

func validCreditPolicy() map[string]any {
	return map[string]any{
		"daily_allowance_pt": "7.000001", "timezone": "Asia/Shanghai", "grant_time": "06:30:00", "daily_validity_seconds": 86400, "billing_window_seconds": 60, "shutdown_grace_seconds": 600,
		"grace_charge_mode": "waived", "restore_action": "cancel_pending_stop", "storage_charge_mode": "retained", "offline_charge_mode": "defer",
	}
}
func encodedPolicy(t *testing.T, p map[string]any) json.RawMessage {
	t.Helper()
	b, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestPolicyValidationRejectsUnconfirmedEconomics(t *testing.T) {
	if _, e := validatePolicy("credit", encodedPolicy(t, validCreditPolicy())); e != nil {
		t.Fatalf("explicit valid policy rejected: %v", e)
	}
	customGrace := validCreditPolicy()
	customGrace["shutdown_grace_seconds"] = 1200
	if _, e := validatePolicy("credit", encodedPolicy(t, customGrace)); e != nil {
		t.Fatalf("explicit custom shutdown grace rejected: %v", e)
	}
	for _, field := range []string{"grace_charge_mode", "restore_action", "storage_charge_mode", "offline_charge_mode"} {
		t.Run("missing_"+field, func(t *testing.T) {
			p := validCreditPolicy()
			delete(p, field)
			if _, e := validatePolicy("credit", encodedPolicy(t, p)); e == nil {
				t.Fatal("missing economic decision received a hidden default")
			}
		})
	}
	cases := []struct {
		name, field string
		value       any
	}{
		{"zero_grace", "shutdown_grace_seconds", 0}, {"excessive_grace", "shutdown_grace_seconds", 86401}, {"invalid_timezone", "timezone", "Unknown/Nowhere"}, {"bad_clock", "grant_time", "24:00:00"}, {"zero_validity", "daily_validity_seconds", 0}, {"negative_window", "billing_window_seconds", -1},
		{"numeric_money", "daily_allowance_pt", 7}, {"excess_precision", "daily_allowance_pt", "0.0000001"}, {"negative_price", "daily_allowance_pt", "-1"}, {"unknown_mode", "offline_charge_mode", "guess"}, {"unknown_key", "allow_negative_balance", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validCreditPolicy()
			p[tc.field] = tc.value
			if _, e := validatePolicy("credit", encodedPolicy(t, p)); e == nil {
				t.Fatalf("invalid %s accepted", tc.field)
			}
		})
	}
	for _, grace := range []string{"waived", "accrue"} {
		for _, restore := range []string{"cancel_pending_stop", "keep_deadline"} {
			for _, storage := range []string{"retained", "running_only", "free"} {
				for _, offline := range []string{"defer", "continue"} {
					p := validCreditPolicy()
					p["grace_charge_mode"] = grace
					p["restore_action"] = restore
					p["storage_charge_mode"] = storage
					p["offline_charge_mode"] = offline
					if _, e := validatePolicy("credit", encodedPolicy(t, p)); e != nil {
						t.Errorf("explicit supported policy combination rejected: %v", e)
					}
				}
			}
		}
	}
}
func TestPricingPolicyIsStrictAndExact(t *testing.T) {
	valid := map[string]any{"cpu_hourly_pt": "0.000001", "memory_gib_hourly_pt": "99999999999999.999999", "disk_gib_hourly_pt": "0"}
	typed, e := validatePolicy("pricing", encodedPolicy(t, valid))
	if e != nil {
		t.Fatal(e)
	}
	p, ok := typed.(PricingConfig)
	if !ok || p.CPU != "0.000001" || p.Memory != "99999999999999.999999" {
		t.Fatalf("exact decimal strings changed: %#v", typed)
	}
	for _, field := range []string{"cpu_hourly_pt", "memory_gib_hourly_pt", "disk_gib_hourly_pt"} {
		for _, bad := range []any{"-0.000001", "1.1234567", 1.25, nil} {
			clone := map[string]any{}
			for k, v := range valid {
				clone[k] = v
			}
			clone[field] = bad
			if _, e = validatePolicy("pricing", encodedPolicy(t, clone)); e == nil {
				t.Errorf("%s=%v accepted", field, bad)
			}
		}
	}
	valid["billing_mode"] = "admin_exempt"
	if _, e = validatePolicy("pricing", encodedPolicy(t, valid)); e == nil {
		t.Fatal("unknown pricing config key accepted")
	}
	if _, e = validatePolicy("arbitrary-policy", json.RawMessage(`{}`)); e == nil {
		t.Fatal("unknown policy kind accepted")
	}
}
func TestMetricsOptionalEngineeringBounds(t *testing.T) {
	p := map[string]any{"raw_retention_days": 7, "rollup_retention_days": 30, "bucket_seconds": 60, "collection_interval_seconds": 5}
	if _, e := validatePolicy("metrics", encodedPolicy(t, p)); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		key   string
		value int
		valid bool
	}{{"collection_timeout_seconds", 1, true}, {"collection_timeout_seconds", 120, true}, {"collection_timeout_seconds", 0, false}, {"collection_timeout_seconds", 121, false}, {"max_concurrent_nodes", 1, true}, {"max_concurrent_nodes", 32, true}, {"max_concurrent_nodes", 0, false}, {"max_concurrent_nodes", 33, false}} {
		clone := map[string]any{}
		for k, v := range p {
			clone[k] = v
		}
		clone[tc.key] = tc.value
		_, e := validatePolicy("metrics", encodedPolicy(t, clone))
		if (e == nil) != tc.valid {
			t.Errorf("%s=%d: validity mismatch: %v", tc.key, tc.value, e)
		}
	}
}

func TestDailyCycleBoundaryAndCalendarTimezone(t *testing.T) {
	cases := []struct{ name, timezone, grant, at, want string }{
		{"before_grant", "Asia/Shanghai", "06:30:00", "2026-10-30T06:29:59.999999+08:00", "2026-10-29T06:30:00+08:00"},
		{"at_grant", "Asia/Shanghai", "06:30:00", "2026-10-30T06:30:00+08:00", "2026-10-30T06:30:00+08:00"},
		{"absolute_time_not_client_date", "Asia/Shanghai", "06:30:00", "2026-10-29T22:30:00Z", "2026-10-30T06:30:00+08:00"},
		{"year_boundary", "Asia/Shanghai", "00:00:00", "2027-01-01T00:00:00+08:00", "2027-01-01T00:00:00+08:00"},
		{"leap_day", "UTC", "06:30:00", "2028-03-01T06:29:59Z", "2028-02-29T06:30:00Z"},
		{"DST_uses_calendar_day", "America/New_York", "06:30:00", "2026-03-08T06:29:59-04:00", "2026-03-07T06:30:00-05:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at, e := time.Parse(time.RFC3339Nano, tc.at)
			if e != nil {
				t.Fatal(e)
			}
			got, e := cycleAt(row{"timezone": tc.timezone, "grant_time": tc.grant}, at)
			if e != nil {
				t.Fatal(e)
			}
			want, e := time.Parse(time.RFC3339Nano, tc.want)
			if e != nil {
				t.Fatal(e)
			}
			if !got.Equal(want) || got.After(at) {
				t.Fatalf("cycle %s, want %s (now %s)", got, want, at)
			}
			if !at.Before(got.AddDate(0, 0, 1)) {
				t.Fatal("cycle is not the latest calendar grant boundary")
			}
		})
	}
	for _, p := range []row{{"timezone": "Not/A_Zone", "grant_time": "00:00:00"}, {"timezone": "UTC", "grant_time": "25:00:00"}} {
		if _, e := cycleAt(p, time.Now()); e == nil {
			t.Errorf("invalid cycle config accepted: %v", p)
		}
	}
}

func TestPolicyDTOAndLargeIDsPreserveExactStrings(t *testing.T) {
	r := row{"id": uint64(9007199254740993), "source_id": uint64(9007199254740995), "revision": uint64(7), "version": "2026.10", "config": []byte(`{"cpu_hourly_pt":"0.000001"}`)}
	out := policyDTO(r)
	for key, want := range map[string]string{"id": "9007199254740993", "revision": "7", "version": "2026.10"} {
		if got := out[key]; got != want {
			t.Errorf("%s=%v, want exact string %s", key, got, want)
		}
	}
	if _, leaked := out["source_id"]; leaked {
		t.Error("policy DTO exposed the internal legacy source_id")
	}
	cfg, ok := out["config"].(map[string]any)
	if !ok || cfg["cpu_hourly_pt"] != "0.000001" {
		t.Fatalf("config did not preserve its exact decimal string: %#v", out["config"])
	}
}

func TestOptionalPolicyFieldsCannotBeNull(t *testing.T) {
	for _, key := range []string{"collection_timeout_seconds", "max_concurrent_nodes"} {
		p := map[string]any{"raw_retention_days": 7, "rollup_retention_days": 30, "bucket_seconds": 60, "collection_interval_seconds": 5, key: nil}
		if _, e := validatePolicy("metrics", encodedPolicy(t, p)); e == nil {
			t.Errorf("explicit null %s silently received a default", key)
		}
	}
}

func TestExhaustionRequiresAnActualPayableInsufficiency(t *testing.T) {
	session := pricedSession("0.000001", "0", "0", "0", 1, "0", "0")
	for _, elapsed := range []uint64{0, 1000000, 1000000000, 1799999999} {
		charge := row{"status": "insufficient", "expected_pt": money(expected(session, elapsed)), "settled_pt": "0.000000"}
		if hasPayableInsufficiency(charge) {
			t.Errorf("nonzero hourly rate falsely exhausted before a payable micro-point at %d microseconds", elapsed)
		}
	}
	first := row{"status": "insufficient", "expected_pt": money(expected(session, 1800000000)), "settled_pt": "0.000000"}
	if !hasPayableInsufficiency(first) {
		t.Fatal("first real unpaid micro-point must establish payable insufficiency")
	}
	first["settled_pt"] = "0.000001"
	if hasPayableInsufficiency(first) {
		t.Fatal("fully paid charge cannot exhaust an account")
	}
	for _, status := range []string{"pending", "settled", "cancelled"} {
		if hasPayableInsufficiency(row{"status": status, "expected_pt": "1.000000", "settled_pt": "0.000000"}) {
			t.Errorf("%s interval is not confirmed payable settlement insufficiency", status)
		}
	}
	if !hasPayableInsufficiency(row{"status": "insufficient", "expected_pt": "99999999999999.999999", "settled_pt": "99999999999999.999998"}) {
		t.Fatal("one exact micro-point of real debt was lost")
	}
}

func completeBindings() map[string]string {
	return map[string]string{"pricing": "1", "gpu-pricing": "2", "credit": "3", "gpu-scheduling": "4", "gpu-idle": "5", "metrics": "6"}
}
func TestPlatformBindingsRequireTheCompleteKindMapping(t *testing.T) {
	if e := validateBindingShape(completeBindings()); e != nil {
		t.Fatalf("complete binding set rejected: %v", e)
	}
	cases := []struct {
		name     string
		bindings map[string]string
	}{{"nil", nil}, {"empty", map[string]string{}}, {"partial", map[string]string{"credit": "3"}}}
	for _, kind := range requiredPolicyKinds {
		p := completeBindings()
		delete(p, kind)
		cases = append(cases, struct {
			name     string
			bindings map[string]string
		}{"missing_" + kind, p})
	}
	extra := completeBindings()
	extra["arbitrary"] = "7"
	cases = append(cases, struct {
		name     string
		bindings map[string]string
	}{"unknown_kind", extra})
	replaced := completeBindings()
	delete(replaced, "metrics")
	replaced["arbitrary"] = "6"
	cases = append(cases, struct {
		name     string
		bindings map[string]string
	}{"unknown_replaces_required", replaced})
	for _, bad := range []string{"", "0", "-1", "1.0", "18446744073709551616"} {
		p := completeBindings()
		p["credit"] = bad
		cases = append(cases, struct {
			name     string
			bindings map[string]string
		}{"invalid_id_" + bad, p})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if e := validateBindingShape(tc.bindings); e == nil {
				t.Fatal("unsafe replacement mapping accepted")
			}
		})
	}
}

func TestPlatformBindingCannotReplaceCurrentPolicyWithFutureVersion(t *testing.T) {
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, zone)
	for _, tc := range []struct {
		name, kind, status string
		effective          any
		valid              bool
	}{
		{"past", "credit", "active", at.Add(-time.Microsecond), true},
		{"effective_now", "credit", "active", at, true},
		{"future", "credit", "active", at.Add(time.Microsecond), false},
		{"missing_effective", "credit", "active", nil, false},
		{"draft", "credit", "draft", at.Add(-time.Hour), false},
		{"retired", "credit", "retired", at.Add(-time.Hour), false},
		{"wrong_kind", "pricing", "active", at.Add(-time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := row{"kind": tc.kind, "status": tc.status, "effective_at": tc.effective}
			e := validateBindingVersion("credit", p, at)
			if (e == nil) != tc.valid {
				t.Fatalf("binding validity mismatch: %v", e)
			}
		})
	}
}

func TestCompletedCreditEventReactivationRequiresFreshLateRunningProof(t *testing.T) {
	deadline := time.Date(2026, 10, 30, 12, 0, 0, 0, zone)
	completed := deadline.Add(time.Minute)
	observed := completed.Add(time.Minute)
	at := observed.Add(time.Second)
	eventBase := row{"status": "completed", "user_id": uint64(42), "exhausted_at": deadline.Add(-600 * time.Second), "shutdown_deadline": deadline, "updated_at": completed}
	instanceBase := row{"user_id": uint64(42), "billing_mode": "credit", "product_type": "elastic", "desired_state": "stopped", "observed_state": "running", "observed_at": observed, "deleted_at": nil}
	if !canReactivateCompletedEvent(eventBase, instanceBase, "0.000000", false, at) {
		t.Fatal("a freshly observed late start escaped its original exhausted deadline")
	}
	clone := func(src row) row {
		out := row{}
		for k, v := range src {
			out[k] = v
		}
		return out
	}
	cases := []struct {
		name                        string
		changeEvent, changeInstance row
		remaining                   string
		replacement                 bool
		now                         time.Time
	}{
		{name: "cancelled", changeEvent: row{"status": "cancelled"}},
		{name: "replacement_event", replacement: true},
		{name: "credit_restored", remaining: "0.000001"},
		{name: "desired_running", changeInstance: row{"desired_state": "running"}},
		{name: "actually_stopped", changeInstance: row{"observed_state": "stopped"}},
		{name: "custom", changeInstance: row{"product_type": "custom"}},
		{name: "admin_exempt", changeInstance: row{"billing_mode": "admin_exempt"}},
		{name: "different_owner", changeInstance: row{"user_id": uint64(43)}},
		{name: "deleted", changeInstance: row{"deleted_at": observed}},
		{name: "stale_before_completion", changeInstance: row{"observed_at": completed.Add(-time.Microsecond)}},
		{name: "before_original_deadline", changeInstance: row{"observed_at": deadline.Add(-time.Microsecond)}},
		{name: "future_observation", changeInstance: row{"observed_at": at.Add(time.Microsecond)}},
		{name: "deadline_not_due", now: deadline.Add(-time.Microsecond)},
		{name: "missing_observation", changeInstance: row{"observed_at": nil}},
		{name: "missing_completion", changeEvent: row{"updated_at": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := clone(eventBase)
			instance := clone(instanceBase)
			for k, v := range tc.changeEvent {
				event[k] = v
			}
			for k, v := range tc.changeInstance {
				instance[k] = v
			}
			left := tc.remaining
			if left == "" {
				left = "0.000000"
			}
			current := tc.now
			if current.IsZero() {
				current = at
			}
			if canReactivateCompletedEvent(event, instance, left, tc.replacement, current) {
				t.Fatal("unsafe completed-event reactivation allowed")
			}
		})
	}
	if eventBase["shutdown_deadline"] != deadline || eventBase["exhausted_at"] != deadline.Add(-600*time.Second) {
		t.Fatal("late-start proof reset the original deadline")
	}
}
