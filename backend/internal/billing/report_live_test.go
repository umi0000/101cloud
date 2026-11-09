package billing

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/shopspring/decimal"
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
	if forecast.Status != status || forecast.Basis != "current_wallet" {
		t.Fatalf("forecast=%#v, want %s/current_wallet", forecast, status)
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

func TestReportForecastSessionOwnershipAndExpiry(t *testing.T) {
	at := reportLiveClock()
	one := billingForecastSession{Session: reportLiveSession(1, "3600", at)}
	two := billingForecastSession{Session: reportLiveSession(2, "7200", at)}
	cases := []struct {
		name     string
		wallet   billingForecastWallet
		sessions []billingForecastSession
		seconds  string
		boundary time.Duration
		status   string
	}{
		{"no funding", billingForecastWallet{}, []billingForecastSession{one}, "0", 0, "exhausted"},
		{"own held plus free", billingForecastWallet{Held: map[uint64]decimal.Decimal{1: dec("3")}, Free: []billingForecastBucket{{Amount: dec("2")}}}, []billingForecastSession{one}, "5", 5*time.Second + time.Microsecond, "estimated"},
		{"foreign held is not shared", billingForecastWallet{Held: map[uint64]decimal.Decimal{2: dec("100")}}, []billingForecastSession{one}, "0", 0, "exhausted"},
		{"whole workload fails at first owner", billingForecastWallet{Held: map[uint64]decimal.Decimal{1: dec("10")}, Free: []billingForecastBucket{{Amount: dec("3")}}}, []billingForecastSession{one, two}, "1", 1500001 * time.Microsecond, "estimated"},
		{"free expires mid forecast", billingForecastWallet{Free: []billingForecastBucket{{Amount: dec("5"), Expires: at.Add(2 * time.Second)}}}, []billingForecastSession{one}, "2", 2 * time.Second, "estimated"},
		{"held survives free expiry", billingForecastWallet{Held: map[uint64]decimal.Decimal{1: dec("5")}, Free: []billingForecastBucket{{Amount: dec("3"), Expires: at.Add(2 * time.Second)}}}, []billingForecastSession{one}, "5", 5*time.Second + time.Microsecond, "estimated"},
		{"FEFO not input order", billingForecastWallet{Free: []billingForecastBucket{{Amount: dec("4")}, {Amount: dec("1"), Expires: at.Add(2 * time.Second)}}}, []billingForecastSession{one}, "5", 5*time.Second + time.Microsecond, "estimated"},
		{"already expired free excluded", billingForecastWallet{Free: []billingForecastBucket{{Amount: dec("100"), Expires: at}}}, []billingForecastSession{one}, "0", 0, "exhausted"},
		{"submicrosecond expiry cannot loop", billingForecastWallet{Free: []billingForecastBucket{{Amount: dec("1"), Expires: at.Add(time.Nanosecond)}}}, []billingForecastSession{one}, "0", time.Microsecond, "estimated"},
		{"own held protects only one rate", billingForecastWallet{Held: map[uint64]decimal.Decimal{1: dec("5"), 2: dec("1")}, Free: []billingForecastBucket{{Amount: dec("100"), Expires: at.Add(250 * time.Millisecond)}}}, []billingForecastSession{one, two}, "0", 500001 * time.Microsecond, "estimated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forecast := simulateBillingForecast(tc.wallet, tc.sessions, at)
			assertReportForecast(t, forecast, tc.status, tc.seconds, at.Add(tc.boundary))
		})
	}
}

func TestReportForecastUsesCumulativeRoundingPhase(t *testing.T) {
	at := reportLiveClock()
	session := reportLiveSession(1, "0.000001", at)
	wallet := billingForecastWallet{Held: map[uint64]decimal.Decimal{1: dec("0.000001")}}
	assertReportForecast(t, simulateBillingForecast(wallet, []billingForecastSession{{Session: session}}, at), "estimated", "5400", at.Add(5400*time.Second))
	assertReportForecast(t, simulateBillingForecast(wallet, []billingForecastSession{{Session: session, BaseMicros: 1800000000}}, at), "estimated", "7200", at.Add(7200*time.Second))
	// All positive sub-micro-Pt hourly resource rates still consume eventually.
	tiny := reportLiveSession(1, "0", at)
	tiny["memory_gib_hourly_pt"] = "0.000001"
	tiny["config_snapshot"] = `{"memory_bytes":"1"}`
	if hourly, positive, valid := billingSessionHourly(tiny); !valid || !positive || !hourly.IsZero() {
		t.Fatalf("tiny exact resource rate was lost: %s/%v/%v", hourly, positive, valid)
	}
	assertReportForecast(t, simulateBillingForecast(billingForecastWallet{}, []billingForecastSession{{Session: tiny}}, at), "exhausted", "0", at)
	numeric := reportLiveSession(1, "0", at)
	numeric["memory_gib_hourly_pt"] = "1"
	numeric["config_snapshot"] = `{"memory_bytes":1073741824}`
	if hourly, positive, valid := billingSessionHourly(numeric); !valid || !positive || money(hourly) != "1.000000" {
		t.Fatalf("numeric resource snapshot was rejected: %s/%v/%v", hourly, positive, valid)
	}
}

func TestReportForecastZeroNegativeUnknownAndOverflow(t *testing.T) {
	at := reportLiveClock()
	zero := reportLiveSession(1, "0", at)
	negative := reportLiveSession(1, "-1", at)
	unknown := reportLiveSession(1, "1", at)
	unknown["config_snapshot"] = `{}`
	one := reportLiveSession(1, "3600", at)
	for _, tc := range []struct {
		name     string
		wallet   billingForecastWallet
		sessions []billingForecastSession
		status   string
	}{
		{"no sessions", billingForecastWallet{}, nil, "not_consuming"},
		{"zero rate", billingForecastWallet{}, []billingForecastSession{{Session: zero}}, "not_consuming"},
		{"negative price", billingForecastWallet{}, []billingForecastSession{{Session: negative}}, "unavailable"},
		{"unknown frozen quantity", billingForecastWallet{}, []billingForecastSession{{Session: unknown}}, "unavailable"},
		{"negative free", billingForecastWallet{Free: []billingForecastBucket{{Amount: dec("-1")}}}, []billingForecastSession{{Session: one}}, "unavailable"},
		{"negative held", billingForecastWallet{Held: map[uint64]decimal.Decimal{1: dec("-1")}}, []billingForecastSession{{Session: one}}, "unavailable"},
		{"counter overflow", billingForecastWallet{Held: map[uint64]decimal.Decimal{1: dec("100")}}, []billingForecastSession{{Session: one, BaseMicros: math.MaxUint64 - 3}}, "unavailable"},
		{"horizon outside duration", billingForecastWallet{Held: map[uint64]decimal.Decimal{1: dec("99999999999999")}}, []billingForecastSession{{Session: reportLiveSession(1, "0.000001", at)}}, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertReportForecast(t, simulateBillingForecast(tc.wallet, tc.sessions, at), tc.status, "", time.Time{})
		})
	}
}

func TestReportForecastDoesNotMutateSnapshot(t *testing.T) {
	at := reportLiveClock()
	wallet := billingForecastWallet{Free: []billingForecastBucket{{Amount: dec("4")}, {Amount: dec("1"), Expires: at.Add(time.Second)}}, Held: map[uint64]decimal.Decimal{1: dec("2")}}
	before := jsonValue(wallet)
	_ = simulateBillingForecast(wallet, []billingForecastSession{{Session: reportLiveSession(1, "3600", at)}}, at)
	if after := jsonValue(wallet); before != after {
		t.Fatalf("simulation changed original wallet: %s != %s", before, after)
	}
}

func TestReportInstanceLiveFrozenRatesAndStoppedTail(t *testing.T) {
	at := reportLiveClock()
	instance := reportLiveInstance(at)
	session := reportLiveSession(1, "3600", at)
	session["last_settled_at"] = at.Add(-2 * time.Second)
	session["metered_microseconds"] = uint64(1000000)
	session["cumulative_expected_pt"] = "1.000000"
	rate, tails, workload, reasons := billingInstanceLive(instance, []row{session}, reportLivePolicy(), nil, true, nil, at)
	if rate.Status != "ready" || money(rate.Hourly) != "3600.000000" || len(workload) != 1 || workload[0].BaseMicros != 3000000 || len(tails) != 1 || len(reasons) != 0 {
		t.Fatalf("frozen rate/tail mismatch: %#v %#v %#v %v", rate, tails, workload, reasons)
	}
	if tails[0].BaseMicros != 1000000 || money(tails[0].BaseExpected) != "1.000000" || !tails[0].From.Equal(at.Add(-2*time.Second)) || !tails[0].To.Equal(at) {
		t.Fatalf("tail lost original cumulative origin: %#v", tails[0])
	}
	instance["observed_state"] = "stopped"
	instance["observed_at"] = at.Add(-time.Second)
	// Desired running does not override fresh stopped evidence.
	rate, tails, workload, reasons = billingInstanceLive(instance, []row{session}, reportLivePolicy(), nil, true, nil, at)
	if rate.Status != "ready" || !rate.Hourly.IsZero() || len(workload) != 0 || len(tails) != 1 || !tails[0].To.Equal(at.Add(-time.Second)) || len(reasons) != 0 {
		t.Fatalf("stopped observation charged beyond its boundary: %#v %#v %#v %v", rate, tails, workload, reasons)
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
			rate, tails, workload, reasons := billingInstanceLive(instance, []row{session}, reportLivePolicy(), nil, tc.online, nil, at)
			if rate.Status != tc.wantStatus || len(tails) != tc.wantTails {
				t.Fatalf("rate=%#v tails=%#v reasons=%v", rate, tails, reasons)
			}
			if tc.wantStatus == "unavailable" && len(reasons) == 0 {
				t.Fatal("unknown observation lost explanation")
			}
			if tc.wantStatus == "ready" && (!rate.Hourly.IsZero() || len(workload) != 0) {
				t.Fatalf("nonbillable session contributes current consumption: %#v %#v", rate, workload)
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
			rate, tails, workload, reasons := billingInstanceLive(reportLiveInstance(at), []row{session}, reportLivePolicy(), event, true, nil, at)
			if rate.Status != "ready" || !rate.Hourly.IsZero() || len(tails) != tc.wantTails || len(workload) != 0 || len(reasons) != 0 {
				t.Fatalf("waiver mismatch: %#v %#v %#v %v", rate, tails, workload, reasons)
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
	rate, tails, workload, reasons := billingInstanceLive(instance, []row{session}, reportLivePolicy(), event, false, nil, at)
	if rate.Status != "ready" || len(tails)+len(workload)+len(reasons) != 0 {
		t.Fatalf("fully waived session unexpectedly unknown: %#v %#v %#v %v", rate, tails, workload, reasons)
	}
}

func TestReportInstanceLiveMultipleFrozenSessions(t *testing.T) {
	at := reportLiveClock()
	base := reportLiveSession(1, "2", at)
	gpu := reportLiveSession(2, "0", at)
	gpu["charge_kind"], gpu["gpu_allocation_id"], gpu["gpu_hourly_pt"] = "gpu", uint64(10), "3"
	rate, _, workload, reasons := billingInstanceLive(reportLiveInstance(at), []row{base, gpu}, reportLivePolicy(), nil, true, []uint64{10}, at)
	if rate.Status != "ready" || money(rate.Hourly) != "5.000000" || len(workload) != 2 || len(reasons) != 0 {
		t.Fatalf("multiple sessions not added from frozen snapshots: %#v %#v %v", rate, workload, reasons)
	}
	missing, _, _, missingReasons := billingInstanceLive(reportLiveInstance(at), []row{base}, reportLivePolicy(), nil, true, []uint64{10}, at)
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
