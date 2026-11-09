package billing

import (
	"encoding/json"
	"math"
	"sort"
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
	var buckets, reservations []row
	if forecast {
		buckets, err = rows(db.Select("id,remaining_pt,reserved_pt,expires_at,valid_from").Order("expires_at IS NULL,expires_at,id"), "credit_buckets", "account_id=? AND (remaining_pt<>0 OR reserved_pt<>0)", account["id"])
		if err != nil {
			return live, err
		}
		reservations, err = rows(db.Select("bucket_id,session_id,reserved_pt,settled_pt,released_pt"), "credit_reservations", "account_id=? AND status='held' AND reserved_pt>settled_pt+released_pt", account["id"])
		if err != nil {
			return live, err
		}
	}
	// Failed creations disappear from consumption/debt immediately, before the
	// asynchronous cancellation/refund worker has touched their historical rows.
	var unpaid []row
	if forecast {
		err = db.Table("usage_charges c").Select("c.session_id,COALESCE(SUM(c.expected_pt-c.settled_pt),0) unpaid_pt,COALESCE(SUM(CASE WHEN c.status='insufficient' THEN c.expected_pt-c.settled_pt ELSE 0 END),0) debt_pt,MAX(CASE WHEN c.status='pending' AND JSON_EXTRACT(c.calculation_snapshot,'$.offline_deferred')=true AND (n.id IS NULL OR n.health<>'online') THEN 1 ELSE 0 END) deferred").Joins("JOIN usage_sessions s ON s.id=c.session_id").Joins("JOIN instances i ON i.id=s.instance_id").Joins("LEFT JOIN nodes n ON n.id=s.node_id").Where("c.account_id=? AND c.status IN ('pending','insufficient') AND c.expected_pt>c.settled_pt AND i.creation_failed_at IS NULL", account["id"]).Group("c.session_id").Find(&unpaid).Error
		if err != nil {
			return live, err
		}
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

	var wallet billingForecastWallet
	var obligations map[uint64]decimal.Decimal
	walletValid, deferred := true, false
	if forecast {
		wallet.Held = make(map[uint64]decimal.Decimal)
		reserved, debt := decimal.Zero, decimal.Zero
		bucketByID := make(map[uint64]row, len(buckets))
		for _, bucket := range buckets {
			bucketByID[platform.Uint(bucket["id"])] = bucket
			remaining, held := dec(bucket["remaining_pt"]), dec(bucket["reserved_pt"])
			reserved = reserved.Add(held)
			if remaining.IsNegative() || held.IsNegative() || held.GreaterThan(remaining) {
				walletValid = false
			}
			expires := stamp(bucket["expires_at"])
			if !stamp(bucket["valid_from"]).After(at) && (expires.IsZero() || expires.After(at)) {
				free := remaining.Sub(held)
				if free.IsPositive() {
					wallet.Free = append(wallet.Free, billingForecastBucket{Amount: free, Expires: expires})
				}
			}
		}
		for _, reservation := range reservations {
			bucket := bucketByID[platform.Uint(reservation["bucket_id"])]
			if bucket == nil {
				walletValid = false
				continue
			}
			if stamp(bucket["valid_from"]).After(at) {
				continue
			}
			id := platform.Uint(reservation["session_id"])
			wallet.Held[id] = wallet.Held[id].Add(dec(reservation["reserved_pt"]).Sub(dec(reservation["settled_pt"])).Sub(dec(reservation["released_pt"])))
		}
		free := wallet.freeTotal()
		obligations = make(map[uint64]decimal.Decimal)
		for _, charge := range unpaid {
			id := platform.Uint(charge["session_id"])
			obligations[id] = dec(charge["unpaid_pt"])
			debt = debt.Add(dec(charge["debt_pt"]))
			deferred = deferred || platform.Uint(charge["deferred"]) != 0
		}
		live.Balance = billingBalanceDTO{AvailablePT: money(free.Sub(debt)), ReservedPT: money(reserved), OutstandingDebtPT: money(debt)}
	}
	var workload []billingForecastSession
	for _, instance := range instances {
		id := platform.Uint(instance["id"])
		ss := byInstance[id]
		policy := policies[platform.Uint(instance["credit_policy_version_id"])]
		online := platform.Text(nodes[platform.Uint(instance["node_id"])]["health"]) == "online"
		rate, tails, consuming, reasons := billingInstanceUsage(instance, ss, policy, eventByInstance[id], online, gpuByInstance[id], at, forecast)
		live.Rates[id] = rate
		for _, reason := range reasons {
			live.addReason(reason)
		}
		for _, tail := range tails {
			live.Tails = append(live.Tails, tail)
			if !forecast {
				continue
			}
			micros, ok := billingElapsedMicros(tail.From, tail.To)
			if !ok || micros > math.MaxUint64-tail.BaseMicros {
				live.addReason("metering_overflow")
				continue
			}
			amount := expected(tail.Session, tail.BaseMicros+micros).Sub(tail.BaseExpected)
			obligations[platform.Uint(tail.Session["id"])] = obligations[platform.Uint(tail.Session["id"])].Add(amount)
		}
		if forecast {
			workload = append(workload, consuming...)
		}
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
	// Unknown tails can consume the same free wallet as a known session. Never
	// forecast just the known subset, or count the public debt-adjusted balance
	// as funding and then subtract that debt a second time.
	switch {
	case !currentKnown || live.Partial:
		live.Forecast = billingForecastUnavailable("current_workload_unavailable")
	case len(workload) == 0:
		live.Forecast = billingForecastDTO{Status: "not_consuming", Basis: "current_wallet"}
	case !walletValid:
		live.Forecast = billingForecastUnavailable("wallet_inconsistent")
	case platform.Text(account["status"]) != "active":
		live.Forecast = billingForecastUnavailable("account_not_active")
	case deferred:
		live.Forecast = billingForecastUnavailable("offline_settlement_deferred")
	default:
		ids := make([]uint64, 0, len(obligations))
		for id := range obligations {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		unpaidCost := false
		for _, id := range ids {
			if !wallet.pay(id, obligations[id]) {
				unpaidCost = true
			}
		}
		live.Forecast = simulateBillingForecast(wallet, workload, at)
		if unpaidCost && live.Forecast.Status != "exhausted" {
			live.Forecast = billingForecastUnavailable("unfunded_existing_usage")
		}
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

// billingInstanceLive mirrors settleSession's waiver test: an interval crossing
// exhausted_at is still billable when its persisted cursor precedes that time.
// A waived *current* rate does not erase that already accrued crossing interval.
func billingInstanceLive(instance row, sessions []row, policy, event row, online bool, gpuIDs []uint64, at time.Time) (billingRate, []billingTail, []billingForecastSession, []string) {
	return billingInstanceUsage(instance, sessions, policy, event, online, gpuIDs, at, true)
}

func billingInstanceUsage(instance row, sessions []row, policy, event row, online bool, gpuIDs []uint64, at time.Time, needForecast bool) (billingRate, []billingTail, []billingForecastSession, []string) {
	rate := billingRate{Status: "ready"}
	var tails []billingTail
	var workload []billingForecastSession
	var reasons []string
	unknown := func(reason string) {
		rate.Status = "unavailable"
		reasons = append(reasons, reason)
	}
	if instance["creation_failed_at"] != nil {
		return rate, tails, workload, reasons
	}
	if platform.Text(instance["billing_mode"]) == "admin_exempt" {
		return rate, tails, workload, reasons
	}
	if instance["deleted_at"] != nil {
		for _, session := range sessions {
			if platform.Text(session["metering_state"]) != "held" && stamp(session["last_settled_at"]).Before(stamp(instance["deleted_at"])) {
				reasons = append(reasons, "deleted_session_unclosed")
				break
			}
		}
		return rate, tails, workload, reasons
	}
	waived := false
	exhausted := time.Time{}
	if event != nil {
		var snapshot row
		if json.Unmarshal([]byte(platform.Text(event["policy_snapshot"])), &snapshot) != nil || snapshot == nil || !in(platform.Text(snapshot["grace_charge_mode"]), "waived", "charged") {
			unknown("enforcement_policy_unavailable")
			return rate, tails, workload, reasons
		}
		exhausted = stamp(event["exhausted_at"])
		if exhausted.IsZero() || exhausted.After(at) {
			unknown("enforcement_boundary_unavailable")
			return rate, tails, workload, reasons
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
			return rate, tails, workload, reasons
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
				if positive && needForecast {
					workload = append(workload, billingForecastSession{Session: session, BaseMicros: base + elapsed})
				}
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
	return rate, tails, workload, reasons
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

type billingForecastBucket struct {
	Amount  decimal.Decimal
	Expires time.Time
}

type billingForecastWallet struct {
	Free []billingForecastBucket
	Held map[uint64]decimal.Decimal
}

type billingForecastSession struct {
	Session row
	// Counter at as_of, after deducting the unclosed tail exactly once.
	BaseMicros uint64
}

func (wallet *billingForecastWallet) freeTotal() decimal.Decimal {
	total := decimal.Zero
	for _, bucket := range wallet.Free {
		total = total.Add(bucket.Amount)
	}
	return total
}

func (wallet *billingForecastWallet) pay(sessionID uint64, amount decimal.Decimal) bool {
	owned := decimal.Min(amount, wallet.Held[sessionID])
	wallet.Held[sessionID] = wallet.Held[sessionID].Sub(owned)
	return wallet.payFree(amount.Sub(owned))
}

func (wallet *billingForecastWallet) payFree(amount decimal.Decimal) bool {
	for index := range wallet.Free {
		paid := decimal.Min(amount, wallet.Free[index].Amount)
		wallet.Free[index].Amount = wallet.Free[index].Amount.Sub(paid)
		amount = amount.Sub(paid)
		if !amount.IsPositive() {
			return true
		}
	}
	return !amount.IsPositive()
}

func billingForecastUnavailable(reason string) billingForecastDTO {
	return billingForecastDTO{Status: "unavailable", Reason: &reason, Basis: "current_wallet"}
}

// Static workload simulation, not a promised stop time: no future grants,
// not-yet-valid buckets, session releases, renewals or workload changes. Held
// money pays only its owner and survives expiry. Free money is consumed FEFO and
// expires at its stored boundary. Demand uses the engine's rounded cumulative
// fee, including each session's existing rounding phase. The returned horizon
// is the first microsecond whose workload demand cannot all be funded; an
// already source-less consuming session is exhausted immediately.
// Seconds is floored to whole seconds; ExhaustsAt retains the exact microsecond.
func simulateBillingForecast(wallet billingForecastWallet, sessions []billingForecastSession, at time.Time) billingForecastDTO {
	if len(sessions) == 0 {
		return billingForecastDTO{Status: "not_consuming", Basis: "current_wallet"}
	}
	// Own the simulation buffers; callers may retain their original snapshot.
	wallet.Free = append([]billingForecastBucket(nil), wallet.Free...)
	held := make(map[uint64]decimal.Decimal, len(wallet.Held))
	for id, amount := range wallet.Held {
		if amount.IsNegative() {
			return billingForecastUnavailable("wallet_inconsistent")
		}
		held[id] = amount
	}
	wallet.Held = held
	sort.SliceStable(wallet.Free, func(i, j int) bool {
		left, right := wallet.Free[i].Expires, wallet.Free[j].Expires
		return !left.IsZero() && (right.IsZero() || left.Before(right))
	})
	limit := uint64(math.MaxInt64 / int64(time.Microsecond))
	active := make([]billingForecastSession, 0, len(sessions))
	for _, session := range sessions {
		_, positive, valid := billingSessionHourly(session.Session)
		if !valid {
			return billingForecastUnavailable("session_rate_unavailable")
		}
		if !positive {
			continue
		}
		if remaining := uint64(math.MaxUint64) - session.BaseMicros; remaining < limit {
			limit = remaining
		}
		active = append(active, session)
	}
	sessions = active
	if len(sessions) == 0 {
		return billingForecastDTO{Status: "not_consuming", Basis: "current_wallet"}
	}
	baseCosts := make([]decimal.Decimal, len(sessions))
	for index, session := range sessions {
		baseCosts[index] = expected(session.Session, session.BaseMicros)
	}
	for _, bucket := range wallet.Free {
		if bucket.Amount.IsNegative() {
			return billingForecastUnavailable("wallet_inconsistent")
		}
	}
	result := func(micros uint64) billingForecastDTO {
		seconds := strconv.FormatUint(micros/1000000, 10)
		exhausts := at.Add(time.Duration(micros) * time.Microsecond)
		if exhausts.Year() > 9999 || exhausts.Year() < 1 {
			return billingForecastUnavailable("forecast_horizon_out_of_range")
		}
		timestamp := exhausts.Format(time.RFC3339Nano)
		status := "estimated"
		if micros == 0 {
			status = "exhausted"
		}
		return billingForecastDTO{Status: status, Seconds: &seconds, ExhaustsAt: &timestamp, Basis: "current_wallet"}
	}
	elapsed := uint64(0)
	for {
		current := at.Add(time.Duration(elapsed) * time.Microsecond)
		for index := range wallet.Free {
			if expiry := wallet.Free[index].Expires; !expiry.IsZero() && !expiry.After(current) {
				wallet.Free[index].Amount = decimal.Zero
			}
		}
		free := wallet.freeTotal()
		if !free.IsPositive() {
			for _, session := range sessions {
				if !wallet.Held[platform.Uint(session.Session["id"])].IsPositive() {
					return result(elapsed)
				}
			}
		}
		next := limit
		for _, bucket := range wallet.Free {
			if !bucket.Amount.IsPositive() || bucket.Expires.IsZero() || !bucket.Expires.After(current) {
				continue
			}
			micros, ok := billingElapsedMicros(at, bucket.Expires)
			if ok && at.Add(time.Duration(micros)*time.Microsecond).Before(bucket.Expires) {
				micros++
			}
			if ok && micros < next {
				next = micros
			}
		}
		needAt := func(target uint64) decimal.Decimal {
			need := decimal.Zero
			for index, session := range sessions {
				cost := expected(session.Session, session.BaseMicros+target).Sub(baseCosts[index])
				need = need.Add(decimal.Max(decimal.Zero, cost.Sub(wallet.Held[platform.Uint(session.Session["id"])])))
			}
			return need
		}
		if needAt(next).GreaterThan(free) {
			low, high := elapsed, next
			for low < high {
				middle := low + (high-low)/2
				if needAt(middle).GreaterThan(free) {
					high = middle
				} else {
					low = middle + 1
				}
			}
			return result(low)
		}
		// Every owner consumes its held credit before sharing the free pool.
		for index, session := range sessions {
			cost := expected(session.Session, session.BaseMicros+next)
			wallet.pay(platform.Uint(session.Session["id"]), cost.Sub(baseCosts[index]))
			baseCosts[index] = cost
		}
		if next == limit {
			return billingForecastUnavailable("forecast_horizon_out_of_range")
		}
		elapsed = next
	}
}
