package billing

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// These queries intentionally do not use the engine's locking balance/debt or
// policy helpers. The caller supplies a repeatable, read-only snapshot and clock.
func loadBillingLive(db *gorm.DB, account row, at time.Time) (billingLive, error) {
	return loadBillingSnapshot(db, account, at, true, true)
}

// loadBillingUsage shares the overview's observation and metering snapshot, but
// never reads wallet state or predicts exhaustion. Rates are optional for trend.
func loadBillingUsage(db *gorm.DB, account row, at time.Time, needRates bool) (billingLive, error) {
	return loadBillingSnapshot(db, account, at, needRates, false)
}

func loadBillingSnapshot(db *gorm.DB, account row, at time.Time, needRates, forecast bool) (billingLive, error) {
	live := billingLive{Rates: make(map[uint64]billingRate), Reasons: []string{}}
	instances, err := rows(db.Select("id,node_id,credit_policy_version_id,creation_failed_at,billing_mode,deleted_at,observed_at,observed_state").Order("id"), "instances", "user_id=?", account["user_id"])
	if err != nil {
		return live, err
	}
	sessions, err := rows(db.Select("id,instance_id,node_id,billing_mode,metering_state,charge_kind,gpu_allocation_id,started_at,last_settled_at,metered_microseconds,cumulative_expected_pt,config_snapshot,cpu_hourly_pt,memory_gib_hourly_pt,disk_gib_hourly_pt,gpu_hourly_pt").Order("id"), "usage_sessions", "account_id=? AND status='active'", account["id"])
	if err != nil {
		return live, err
	}
	var buckets []row
	if forecast {
		buckets, err = rows(db.Select("remaining_pt,reserved_pt,expires_at,valid_from"), "credit_buckets", "account_id=? AND (remaining_pt<>0 OR reserved_pt<>0)", account["id"])
		if err != nil {
			return live, err
		}
	}
	// Failed creations disappear from consumption/debt immediately, before the
	// asynchronous cancellation/refund worker has touched their historical rows.
	var debt decimal.Decimal
	if forecast {
		var total struct{ DebtPT decimal.Decimal }
		err = db.Table("usage_charges c").Select("COALESCE(SUM(c.expected_pt-c.settled_pt),0) debt_pt").Joins("JOIN usage_sessions s ON s.id=c.session_id").Joins("JOIN instances i ON i.id=s.instance_id").Where("c.account_id=? AND c.status='insufficient' AND c.expected_pt>c.settled_pt AND i.creation_failed_at IS NULL", account["id"]).Scan(&total).Error
		if err != nil {
			return live, err
		}
		debt = total.DebtPT
	}
	byInstance := make(map[uint64][]row)
	instanceByID := make(map[uint64]row, len(instances))
	nodeIDs, policyIDs := make([]uint64, 0), make([]uint64, 0)
	nodeSeen, policySeen := make(map[uint64]bool), make(map[uint64]bool)
	for _, instance := range instances {
		id := platform.Uint(instance["id"])
		instanceByID[id] = instance
		live.Rates[id] = billingRate{Status: "ready"}
		if instance["creation_failed_at"] == nil && instance["deleted_at"] == nil && platform.Text(instance["billing_mode"]) != "admin_exempt" {
			nodeID, policyID := platform.Uint(instance["node_id"]), platform.Uint(instance["credit_policy_version_id"])
			if !nodeSeen[nodeID] {
				nodeIDs = append(nodeIDs, nodeID)
				nodeSeen[nodeID] = true
			}
			if !policySeen[policyID] {
				policyIDs = append(policyIDs, policyID)
				policySeen[policyID] = true
			}
		}
	}
	for _, session := range sessions {
		id := platform.Uint(session["instance_id"])
		byInstance[id] = append(byInstance[id], session)
		if instanceByID[id] == nil {
			live.addReason("session_instance_unavailable")
		}
	}
	nodes := make(map[uint64]row)
	policies := make(map[uint64]row)
	if len(nodeIDs) > 0 {
		var found []row
		if err = db.Table("nodes").Select("id,health").Where("id IN ?", nodeIDs).Find(&found).Error; err != nil {
			return live, err
		}
		for _, node := range found {
			nodes[platform.Uint(node["id"])] = node
		}
		found = nil
		if err = db.Table("policy_versions").Select("source_id,config").Where("kind='credit' AND source_id IN ?", policyIDs).Find(&found).Error; err != nil {
			return live, err
		}
		for _, policy := range found {
			var config row
			if json.Unmarshal([]byte(platform.Text(policy["config"])), &config) == nil && config != nil {
				policies[platform.Uint(policy["source_id"])] = config
			}
		}
	}
	var events []row
	if err = db.Table("credit_enforcements e").Select("e.id,m.instance_id,e.exhausted_at,e.policy_snapshot").Joins("JOIN credit_enforcement_instances m ON m.enforcement_id=e.id").Where("e.account_id=? AND e.status IN ('grace','stopping','unknown')", account["id"]).Order("e.id").Find(&events).Error; err != nil {
		return live, err
	}
	eventByInstance := make(map[uint64]row)
	for _, event := range events {
		id := platform.Uint(event["instance_id"])
		if previous := eventByInstance[id]; previous != nil && platform.Uint(previous["id"]) != platform.Uint(event["id"]) {
			live.addReason("enforcement_ambiguous")
		}
		eventByInstance[id] = event
	}
	var allocations []row
	if err = db.Table("gpu_allocations").Select("id,instance_id").Where("user_id=? AND status='active' AND configured=TRUE", account["user_id"]).Find(&allocations).Error; err != nil {
		return live, err
	}
	gpuByInstance := make(map[uint64][]uint64)
	for _, allocation := range allocations {
		id := platform.Uint(allocation["instance_id"])
		gpuByInstance[id] = append(gpuByInstance[id], platform.Uint(allocation["id"]))
	}

	walletValid := true
	available := decimal.Zero
	if forecast {
		reserved := decimal.Zero
		for _, bucket := range buckets {
			remaining, held := dec(bucket["remaining_pt"]), dec(bucket["reserved_pt"])
			reserved = reserved.Add(held)
			if remaining.IsNegative() || held.IsNegative() || held.GreaterThan(remaining) {
				walletValid = false
			}
			expires := stamp(bucket["expires_at"])
			if !stamp(bucket["valid_from"]).After(at) && (expires.IsZero() || expires.After(at)) {
				available = available.Add(remaining.Sub(held))
			}
		}
		available = available.Sub(debt)
		live.Balance = billingBalanceDTO{AvailablePT: money(available), ReservedPT: money(reserved), OutstandingDebtPT: money(debt)}
	}
	for _, instance := range instances {
		id := platform.Uint(instance["id"])
		ss := byInstance[id]
		policy := policies[platform.Uint(instance["credit_policy_version_id"])]
		online := platform.Text(nodes[platform.Uint(instance["node_id"])]["health"]) == "online"
		rate, tails, reasons := billingInstanceUsage(instance, ss, policy, eventByInstance[id], online, gpuByInstance[id], at)
		live.Rates[id] = rate
		for _, reason := range reasons {
			live.addReason(reason)
		}
		live.Tails = append(live.Tails, tails...)
	}
	current := decimal.Zero
	currentKnown := true
	for _, rate := range live.Rates {
		current = current.Add(rate.Hourly)
		currentKnown = currentKnown && rate.Status == "ready"
	}
	for _, session := range sessions {
		if instanceByID[platform.Uint(session["instance_id"])] == nil {
			currentKnown = false
		}
	}
	if currentKnown {
		value := money(current)
		live.Current = billingCurrentDTO{HourlyPT: &value, Status: "ready"}
	} else {
		reason := "current_workload_unavailable"
		live.Current = billingCurrentDTO{Status: "unavailable", Reason: &reason}
	}
	if !forecast {
		// Trend still computes the same quality decisions as overview, but does
		// not expose unused rates or perform wallet/forecast work.
		if !needRates {
			live.Rates = nil
			live.Current = billingCurrentDTO{}
		}
		return live, nil
	}
	// Use exactly the public available balance and current total rate. Historical
	// partial usage, pending costs and unclosed tails do not reduce it again.
	switch {
	case !currentKnown:
		live.Forecast = billingForecastUnavailable("current_workload_unavailable")
	case !walletValid:
		live.Forecast = billingForecastUnavailable("wallet_inconsistent")
	case platform.Text(account["status"]) != "active":
		live.Forecast = billingForecastUnavailable("account_not_active")
	default:
		live.Forecast = billingStaticForecast(available, current, at)
	}
	return live, nil
}

func (live *billingLive) addReason(reason string) {
	live.Partial = true
	for _, existing := range live.Reasons {
		if existing == reason {
			return
		}
	}
	live.Reasons = append(live.Reasons, reason)
}

// billingInstanceUsage mirrors settleSession's waiver test: an interval crossing
// exhausted_at is still billable when its persisted cursor precedes that time.
// A waived *current* rate does not erase that already accrued crossing interval.
func billingInstanceUsage(instance row, sessions []row, policy, event row, online bool, gpuIDs []uint64, at time.Time) (billingRate, []billingTail, []string) {
	rate := billingRate{Status: "ready"}
	var tails []billingTail
	var reasons []string
	unknown := func(reason string) {
		rate.Status = "unavailable"
		reasons = append(reasons, reason)
	}
	if instance["creation_failed_at"] != nil {
		return rate, tails, reasons
	}
	if platform.Text(instance["billing_mode"]) == "admin_exempt" {
		return rate, tails, reasons
	}
	if instance["deleted_at"] != nil {
		for _, session := range sessions {
			if platform.Text(session["metering_state"]) != "held" && stamp(session["last_settled_at"]).Before(stamp(instance["deleted_at"])) {
				reasons = append(reasons, "deleted_session_unclosed")
				break
			}
		}
		return rate, tails, reasons
	}
	waived := false
	exhausted := time.Time{}
	if event != nil {
		var snapshot row
		if json.Unmarshal([]byte(platform.Text(event["policy_snapshot"])), &snapshot) != nil || snapshot == nil || !in(platform.Text(snapshot["grace_charge_mode"]), "waived", "charged") {
			unknown("enforcement_policy_unavailable")
			return rate, tails, reasons
		}
		exhausted = stamp(event["exhausted_at"])
		if exhausted.IsZero() || exhausted.After(at) {
			unknown("enforcement_boundary_unavailable")
			return rate, tails, reasons
		}
		waived = platform.Text(snapshot["grace_charge_mode"]) == "waived"
	}
	if waived {
		allWaived := true
		for _, session := range sessions {
			if platform.Text(session["metering_state"]) != "held" && platform.Text(session["billing_mode"]) != "admin_exempt" && stamp(session["last_settled_at"]).Before(exhausted) {
				allWaived = false
			}
		}
		if allWaived {
			return rate, tails, reasons
		}
	}
	fresh := freshUsageObservation(instance, at)
	state := platform.Text(instance["observed_state"])
	knownState := fresh && online && in(state, "running", "stopped")
	kinds := make(map[string]bool)
	gpus := make(map[uint64]bool)
	for _, session := range sessions {
		if platform.Text(session["billing_mode"]) == "admin_exempt" {
			if knownState && state == "running" && platform.Text(session["metering_state"]) == "running" {
				kinds[platform.Text(session["charge_kind"])] = true
				gpus[platform.Uint(session["gpu_allocation_id"])] = true
			}
			continue
		}
		if platform.Text(session["metering_state"]) == "held" {
			continue
		}
		if platform.Text(session["metering_state"]) != "running" {
			unknown("metering_state_unavailable")
			continue
		}
		if !knownState || stamp(instance["observed_at"]).Before(stamp(session["started_at"])) {
			if !online && platform.Text(policy["offline_charge_mode"]) == "defer" {
				unknown("offline_usage_deferred")
			} else {
				unknown("usage_observation_unavailable")
			}
			continue
		}
		if policy == nil {
			unknown("session_policy_unavailable")
			continue
		}
		hourly, positive, valid := billingSessionHourly(session)
		if !valid {
			unknown("session_rate_unavailable")
			continue
		}
		from, to := stamp(session["last_settled_at"]), at
		if state == "stopped" {
			to = stamp(instance["observed_at"])
		}
		base := platform.Uint(session["metered_microseconds"])
		baseExpected := dec(session["cumulative_expected_pt"])
		if from.IsZero() || from.After(at) || baseExpected.IsNegative() || !expected(session, base).Equal(baseExpected) {
			unknown("session_cursor_unavailable")
			continue
		}
		elapsed := uint64(0)
		if to.After(from) && !(waived && !from.Before(exhausted)) {
			var ok bool
			elapsed, ok = billingElapsedMicros(from, to)
			if !ok || elapsed > math.MaxUint64-base {
				unknown("metering_overflow")
				continue
			}
			if positive {
				tails = append(tails, billingTail{Session: session, From: from, To: to, BaseMicros: base, BaseExpected: baseExpected})
			}
		}
		if state == "running" {
			kinds[platform.Text(session["charge_kind"])] = true
			gpus[platform.Uint(session["gpu_allocation_id"])] = true
			if !waived {
				rate.Hourly = rate.Hourly.Add(hourly)
			}
		}
	}
	if !knownState {
		// Held reservations alone are not evidence of runtime. A fresh stopped
		// observation is unnecessary to establish that those sessions charge 0.
		allHeld := len(sessions) > 0
		for _, session := range sessions {
			allHeld = allHeld && (platform.Text(session["metering_state"]) == "held" || platform.Text(session["billing_mode"]) == "admin_exempt")
		}
		if !allHeld && rate.Status == "ready" {
			unknown("usage_observation_unavailable")
		}
	} else if state == "running" && !waived {
		if !kinds["base_compute"] || policy == nil || (platform.Text(policy["storage_charge_mode"]) != "free" && !kinds["storage"]) {
			unknown("workload_not_reconciled")
		}
		for _, id := range gpuIDs {
			if !gpus[id] {
				unknown("workload_not_reconciled")
				break
			}
		}
	}
	return rate, tails, reasons
}

// Validate rather than turning missing/corrupt frozen hardware or prices into 0.
// expected remains the single authority for fee calculation and rounding.
func billingSessionHourly(session row) (decimal.Decimal, bool, bool) {
	var config row
	decoder := json.NewDecoder(strings.NewReader(platform.Text(session["config_snapshot"])))
	decoder.UseNumber()
	if decoder.Decode(&config) != nil || config == nil {
		return decimal.Zero, false, false
	}
	positive := false
	for _, field := range []struct{ price, dimension string }{{"cpu_hourly_pt", "vcpus"}, {"memory_gib_hourly_pt", "memory_bytes"}, {"disk_gib_hourly_pt", "disk_bytes"}, {"gpu_hourly_pt", ""}} {
		price, err := decimal.NewFromString(platform.Text(session[field.price]))
		if err != nil || price.IsNegative() {
			return decimal.Zero, false, false
		}
		if !price.IsPositive() {
			continue
		}
		if field.dimension == "" {
			positive = true
			continue
		}
		text := platform.Text(config[field.dimension])
		value, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return decimal.Zero, false, false
		}
		positive = positive || value > 0
	}
	return expected(session, 3600000000), positive, true
}

func billingElapsedMicros(from, to time.Time) (uint64, bool) {
	if to.Before(from) {
		return 0, false
	}
	duration := to.Sub(from)
	if duration == time.Duration(math.MaxInt64) {
		return 0, false
	}
	return uint64(duration.Microseconds()), true
}

const billingForecastBasis = "current_balance_at_current_rate"

func billingForecastUnavailable(reason string) billingForecastDTO {
	return billingForecastDTO{Status: "unavailable", Reason: &reason, Basis: billingForecastBasis}
}

// billingStaticForecast is a balance/rate conversion, not an actual stop time.
// QuoRem truncates the exact decimal ratio without division rounding or floats.
// Future grants, expiry, held-credit ownership and fee rounding phases have no
// role here: available already excludes reservations and reported debt once.
func billingStaticForecast(available, hourly decimal.Decimal, at time.Time) billingForecastDTO {
	if hourly.IsNegative() {
		return billingForecastUnavailable("current_workload_unavailable")
	}
	if hourly.IsZero() {
		return billingForecastDTO{Status: "not_consuming", Basis: billingForecastBasis}
	}
	seconds := decimal.Zero
	status := "exhausted"
	if available.IsPositive() {
		seconds, _ = available.Mul(decimal.NewFromInt(3600)).QuoRem(hourly, 0)
		status = "estimated"
	}
	value := seconds.String()
	result := billingForecastDTO{Status: status, Seconds: &value, Basis: billingForecastBasis}
	// Avoid time.Duration's ~292-year limit and never discard a valid duration
	// merely because RFC3339 cannot represent the corresponding calendar time.
	whole := seconds.Coefficient()
	if whole.IsInt64() {
		offset := whole.Int64()
		latest := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC).Unix()
		if at.Year() >= 1 && at.Year() <= 9999 && offset <= latest-at.Unix() {
			end := time.Unix(at.Unix()+offset, int64(at.Nanosecond())).In(at.Location())
			if end.Year() >= 1 && end.Year() <= 9999 {
				timestamp := end.Format(time.RFC3339Nano)
				result.ExhaustsAt = &timestamp
			}
		}
	}
	return result
}
