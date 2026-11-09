package billing

import (
	"reflect"
	"testing"
	"time"
)

func reportLiveClock() time.Time {
	return time.Date(2026, 11, 9, 12, 0, 0, 0, zone)
}

func reportLiveSession(id uint64, hourly string, at time.Time) row {
	session := pricedSession(hourly, "0", "0", "0", 1, "0", "0")
	for key, value := range (row{"id": id, "instance_id": uint64(1), "node_id": uint64(1), "charge_kind": "base_compute", "billing_mode": "credit", "status": "active", "metering_state": "running", "started_at": at.Add(-time.Hour), "last_settled_at": at, "metered_microseconds": uint64(0), "cumulative_expected_pt": "0.000000"}) {
		session[key] = value
	}
	return session
}

func reportLiveInstance(at time.Time) row {
	return row{"id": uint64(1), "billing_mode": "credit", "observed_state": "running", "desired_state": "running", "observed_at": at, "cpu_hourly_pt": "999.000000"}
}

func reportLivePolicy() row {
	return row{"storage_charge_mode": "free", "offline_charge_mode": "defer"}
}

func assertReportForecast(t *testing.T, forecast billingForecastDTO, status, seconds string, boundary time.Time) {
	t.Helper()
	if forecast.Status != status || forecast.Basis != billingForecastBasis {
		t.Fatalf("forecast=%#v, want %s/%s", forecast, status, billingForecastBasis)
	}
	if seconds == "" {
		if forecast.Seconds != nil || forecast.ExhaustsAt != nil {
			t.Fatalf("non-numeric forecast invented a horizon: %#v", forecast)
		}
		return
	}
	if forecast.Seconds == nil || *forecast.Seconds != seconds || forecast.ExhaustsAt == nil {
		t.Fatalf("forecast=%#v, want seconds %s", forecast, seconds)
	}
	got, err := time.Parse(time.RFC3339Nano, *forecast.ExhaustsAt)
	if err != nil || !got.Equal(boundary) {
		t.Fatalf("exhausts_at=%v, want %s: %v", forecast.ExhaustsAt, boundary, err)
	}
}

func TestReportForecastCurrentBalanceAtCurrentRate(t *testing.T) {
	at := reportLiveClock()
	for _, tc := range []struct {
		name      string
		available string
		hourly    string
		seconds   string
		status    string
	}{
		{"fifty hours crosses daily refresh", "200.000000", "4.000000", "180000", "estimated"},
		{"raw balance not displayed rounding", "199.900000", "4.000000", "179910", "estimated"},
		{"raw six-place balance", "199.999999", "4.000000", "179999", "estimated"},
		{"fraction floors", "0.000001", "0.007000", "0", "estimated"},
		{"exact second boundary", "0.000001", "0.003600", "1", "estimated"},
		{"below second boundary", "0.000001", "0.003601", "0", "estimated"},
		{"division must not round across integer", "0.999999999999999999999999999999", "3600", "0", "estimated"},
		{"large exact integer above float range", "9007199254740993.000001", "3600", "9007199254740993", "estimated"},
		{"large balance tiny rate", "99999999999999.999999", "0.000001", "359999999999999999996400", "estimated"},
		{"zero available", "0", "4", "0", "exhausted"},
		{"negative available", "-1", "4", "0", "exhausted"},
		{"zero rate", "200", "0", "", "not_consuming"},
		{"zero rate with debt", "-1", "0", "", "not_consuming"},
		{"invalid negative rate", "200", "-1", "", "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forecast := billingStaticForecast(dec(tc.available), dec(tc.hourly), at)
			if forecast.Status != tc.status || forecast.Basis != billingForecastBasis || (forecast.Reason != nil) != (tc.status == "unavailable") {
				t.Fatalf("forecast=%#v, want %s/%s", forecast, tc.status, billingForecastBasis)
			}
			if tc.seconds == "" {
				assertReportForecast(t, forecast, tc.status, "", time.Time{})
				return
			}
			if forecast.Seconds == nil || *forecast.Seconds != tc.seconds {
				t.Fatalf("seconds=%v, want exact %s", forecast.Seconds, tc.seconds)
			}
			seconds := dec(tc.seconds)
			if seconds.GreaterThan(dec("300000000000")) {
				if forecast.ExhaustsAt != nil {
					t.Fatalf("out-of-calendar duration invented timestamp: %#v", forecast)
				}
				return
			}
			assertReportForecast(t, forecast, tc.status, tc.seconds, at.Add(time.Duration(seconds.IntPart())*time.Second))
		})
	}
}

func TestReportForecastCalendarBeyondDurationAndOverflow(t *testing.T) {
	at := reportLiveClock().Add(123456 * time.Microsecond)
	// More than time.Duration's 292 years still has a valid RFC3339 timestamp.
	assertReportForecast(t, billingStaticForecast(dec("10000000000"), dec("3600"), at), "estimated", "10000000000", time.Unix(at.Unix()+10000000000, int64(at.Nanosecond())).In(zone))
	last := time.Date(9999, 12, 31, 23, 59, 59, 123456000, zone)
	assertReportForecast(t, billingStaticForecast(dec("1"), dec("3600"), last.Add(-time.Second)), "estimated", "1", last)
	overflow := billingStaticForecast(dec("1"), dec("3600"), last)
	if overflow.Status != "estimated" || overflow.Seconds == nil || *overflow.Seconds != "1" || overflow.ExhaustsAt != nil || overflow.Reason != nil {
		t.Fatalf("calendar overflow discarded computable duration: %#v", overflow)
	}
}

func TestReportSessionHourlyValidatesFrozenRates(t *testing.T) {
	at := reportLiveClock()
	// Current hourly_pt is a six-place engine value, not a future fee-rounding
	// simulation; positive sub-micro-Pt resources can therefore report zero.
	tiny := reportLiveSession(1, "0", at)
	tiny["memory_gib_hourly_pt"] = "0.000001"
	tiny["config_snapshot"] = `{"memory_bytes":"1"}`
	if hourly, positive, valid := billingSessionHourly(tiny); !valid || !positive || !hourly.IsZero() {
		t.Fatalf("tiny resource rate was lost: %s/%v/%v", hourly, positive, valid)
	}
	numeric := reportLiveSession(1, "0", at)
	numeric["memory_gib_hourly_pt"] = "1"
	numeric["config_snapshot"] = `{"memory_bytes":1073741824}`
	if hourly, positive, valid := billingSessionHourly(numeric); !valid || !positive || money(hourly) != "1.000000" {
		t.Fatalf("numeric resource snapshot was rejected: %s/%v/%v", hourly, positive, valid)
	}
	for _, session := range []row{reportLiveSession(1, "-1", at), {"cpu_hourly_pt": "1", "config_snapshot": `{}`}} {
		if _, _, valid := billingSessionHourly(session); valid {
			t.Fatalf("invalid frozen rate accepted: %#v", session)
		}
	}
}

func TestReportInstanceLiveFrozenRatesAndStoppedTail(t *testing.T) {
	at := reportLiveClock()
	instance := reportLiveInstance(at)
	session := reportLiveSession(1, "3600", at)
	session["last_settled_at"] = at.Add(-2 * time.Second)
	session["metered_microseconds"] = uint64(1000000)
	session["cumulative_expected_pt"] = "1.000000"
	rate, tails, reasons := billingInstanceUsage(instance, []row{session}, reportLivePolicy(), nil, true, nil, at)
	if rate.Status != "ready" || money(rate.Hourly) != "3600.000000" || len(tails) != 1 || len(reasons) != 0 {
		t.Fatalf("frozen rate/tail mismatch: %#v %#v %v", rate, tails, reasons)
	}
	if tails[0].BaseMicros != 1000000 || money(tails[0].BaseExpected) != "1.000000" || !tails[0].From.Equal(at.Add(-2*time.Second)) || !tails[0].To.Equal(at) {
		t.Fatalf("tail lost original cumulative origin: %#v", tails[0])
	}
	instance["observed_state"] = "stopped"
	instance["observed_at"] = at.Add(-time.Second)
	// Desired running does not override fresh stopped evidence.
	rate, tails, reasons = billingInstanceUsage(instance, []row{session}, reportLivePolicy(), nil, true, nil, at)
	if rate.Status != "ready" || !rate.Hourly.IsZero() || len(tails) != 1 || !tails[0].To.Equal(at.Add(-time.Second)) || len(reasons) != 0 {
		t.Fatalf("stopped observation charged beyond its boundary: %#v %#v %v", rate, tails, reasons)
	}
}

func TestReportInstanceLiveHeldExemptFreeFailureAndUncertainty(t *testing.T) {
	at := reportLiveClock()
	for _, tc := range []struct {
		name           string
		changeInstance row
		changeSession  row
		online         bool
		wantStatus     string
		wantTails      int
	}{
		{"held stopped", row{"observed_state": "stopped"}, row{"metering_state": "held"}, true, "ready", 0},
		{"held unknown", row{"observed_state": "unknown", "observed_at": nil}, row{"metering_state": "held"}, false, "ready", 0},
		{"held running not reconciled", nil, row{"metering_state": "held"}, true, "unavailable", 0},
		{"instance exempt", row{"billing_mode": "admin_exempt", "observed_at": nil}, nil, false, "ready", 0},
		{"free rate", nil, row{"cpu_hourly_pt": "0"}, true, "ready", 0},
		{"creation failed", row{"creation_failed_at": at}, nil, true, "ready", 0},
		{"deleted active cursor", row{"deleted_at": at}, nil, true, "ready", 0},
		{"offline deferred", nil, nil, false, "unavailable", 0},
		{"stale", row{"observed_at": at.Add(-time.Minute - time.Microsecond)}, nil, true, "unavailable", 0},
		{"future observation", row{"observed_at": at.Add(time.Microsecond)}, nil, true, "unavailable", 0},
		{"unknown", row{"observed_state": "unknown"}, nil, true, "unavailable", 0},
		{"stopped before session", row{"observed_state": "stopped", "observed_at": at.Add(-time.Second)}, row{"started_at": at}, true, "unavailable", 0},
		{"cursor inconsistent", nil, row{"cumulative_expected_pt": "1"}, true, "unavailable", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance, session := reportLiveInstance(at), reportLiveSession(1, "3600", at)
			session["last_settled_at"] = at.Add(-time.Second)
			for key, value := range tc.changeInstance {
				instance[key] = value
			}
			for key, value := range tc.changeSession {
				session[key] = value
			}
			rate, tails, reasons := billingInstanceUsage(instance, []row{session}, reportLivePolicy(), nil, tc.online, nil, at)
			if rate.Status != tc.wantStatus || len(tails) != tc.wantTails {
				t.Fatalf("rate=%#v tails=%#v reasons=%v", rate, tails, reasons)
			}
			if tc.wantStatus == "unavailable" && len(reasons) == 0 {
				t.Fatal("unknown observation lost explanation")
			}
			if tc.wantStatus == "ready" && !rate.Hourly.IsZero() {
				t.Fatalf("nonbillable session contributes current consumption: %#v", rate)
			}
		})
	}
}

func TestReportInstanceLiveWaiverMatchesEngineCursorRule(t *testing.T) {
	at := reportLiveClock()
	exhausted := at.Add(-time.Second)
	event := row{"exhausted_at": exhausted, "policy_snapshot": `{"grace_charge_mode":"waived"}`}
	for _, tc := range []struct {
		name      string
		from      time.Time
		wantTails int
	}{
		{"after exhaustion", exhausted, 0},
		{"crossing exhaustion", exhausted.Add(-time.Second), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := reportLiveSession(1, "3600", at)
			session["last_settled_at"] = tc.from
			rate, tails, reasons := billingInstanceUsage(reportLiveInstance(at), []row{session}, reportLivePolicy(), event, true, nil, at)
			if rate.Status != "ready" || !rate.Hourly.IsZero() || len(tails) != tc.wantTails || len(reasons) != 0 {
				t.Fatalf("waiver mismatch: %#v %#v %v", rate, tails, reasons)
			}
			if len(tails) == 1 && !tails[0].To.Equal(at) {
				t.Fatal("crossing interval was incorrectly clipped at exhaustion")
			}
		})
	}
	// Fully waived sessions need no invented native observation to know cost=0.
	session := reportLiveSession(1, "3600", at)
	instance := reportLiveInstance(at)
	instance["observed_at"] = nil
	rate, tails, reasons := billingInstanceUsage(instance, []row{session}, reportLivePolicy(), event, false, nil, at)
	if rate.Status != "ready" || len(tails)+len(reasons) != 0 {
		t.Fatalf("fully waived session unexpectedly unknown: %#v %#v %v", rate, tails, reasons)
	}
}

func TestReportInstanceLiveMultipleFrozenSessions(t *testing.T) {
	at := reportLiveClock()
	base := reportLiveSession(1, "2", at)
	gpu := reportLiveSession(2, "0", at)
	gpu["charge_kind"], gpu["gpu_allocation_id"], gpu["gpu_hourly_pt"] = "gpu", uint64(10), "3"
	rate, _, reasons := billingInstanceUsage(reportLiveInstance(at), []row{base, gpu}, reportLivePolicy(), nil, true, []uint64{10}, at)
	if rate.Status != "ready" || money(rate.Hourly) != "5.000000" || len(reasons) != 0 {
		t.Fatalf("multiple sessions not added from frozen snapshots: %#v %v", rate, reasons)
	}
	missing, _, missingReasons := billingInstanceUsage(reportLiveInstance(at), []row{base}, reportLivePolicy(), nil, true, []uint64{10}, at)
	if missing.Status != "unavailable" || reflect.DeepEqual(missingReasons, []string{}) {
		t.Fatalf("missing active GPU silently undercounted: %#v %v", missing, missingReasons)
	}
}

func TestReportElapsedMicrosRejectsOverflow(t *testing.T) {
	at := reportLiveClock()
	if _, ok := billingElapsedMicros(at, at.Add(-time.Microsecond)); ok {
		t.Fatal("negative duration became unsigned")
	}
	if _, ok := billingElapsedMicros(time.Date(1600, 1, 1, 0, 0, 0, 0, zone), at); ok {
		t.Fatal("saturated duration was treated as a real interval")
	}
}
