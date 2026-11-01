package billing

import (
	"context"
	"encoding/json"
	"math/big"
	"strconv"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Every entry point runs in the caller's SQL transaction. Account -> sessions ->
// reservations -> buckets is the financial lock order; no agent I/O occurs here.
func AdmitInstance(ctx context.Context, db *gorm.DB, instance row, action string) error {
	if platform.Text(instance["billing_mode"]) == "admin_exempt" {
		return nil
	}
	ac, e := account(db.WithContext(ctx), platform.Uint(instance["user_id"]), true)
	if e != nil {
		return e
	}
	if platform.Text(ac["status"]) != "active" {
		return conflict("credit_exhausted", "credit account is not active")
	}
	var n int64
	if e = db.Table("credit_enforcements").Where("account_id = ? AND status IN ('grace','stopping','unknown')", ac["id"]).Count(&n).Error; e != nil {
		return e
	}
	if n > 0 {
		return conflict("credit_exhausted", "a credit exhaustion deadline is active; new paid resources are blocked")
	}
	p, e := accountPolicy(db, ac)
	if e != nil {
		return e
	}
	left, held, e := balances(db, platform.Uint(ac["id"]))
	if e != nil {
		return e
	}
	rate := row{"config_snapshot": jsonValue(row{"vcpus": instance["vcpus"], "memory_bytes": sid(instance["memory_bytes"]), "disk_bytes": sid(instance["disk_bytes"])}), "cpu_hourly_pt": instance["cpu_hourly_pt"], "memory_gib_hourly_pt": instance["memory_gib_hourly_pt"], "disk_gib_hourly_pt": "0", "gpu_hourly_pt": "0"}
	if platform.Text(p["storage_charge_mode"]) != "free" {
		rate["disk_gib_hourly_pt"] = instance["disk_gib_hourly_pt"]
	}
	required := expected(rate, platform.Uint(p["billing_window_seconds"])*1000000)
	var gs []row
	if e = db.Table("gpu_allocations").Where("instance_id = ? AND status IN ('reserved','attaching','active','releasing','unknown')", instance["id"]).Find(&gs).Error; e != nil {
		return e
	}
	for _, g := range gs {
		required = required.Add(dec(g["gpu_hourly_pt"]).Mul(decimal.NewFromInt(int64(platform.Uint(p["billing_window_seconds"])))).DivRound(decimal.NewFromInt(3600), 6))
	}
	if extra := dec(instance["additional_gpu_hourly_pt"]); extra.IsPositive() {
		required = required.Add(extra.Mul(decimal.NewFromInt(int64(platform.Uint(p["billing_window_seconds"])))).DivRound(decimal.NewFromInt(3600), 6))
	}
	var owned struct{ Held string }
	if e = db.Table("credit_reservations r").Select("COALESCE(SUM(r.reserved_pt-r.settled_pt-r.released_pt),0) held").Joins("JOIN usage_sessions s ON s.id=r.session_id").Where("s.instance_id = ? AND r.status = 'held'", instance["id"]).Scan(&owned).Error; e != nil {
		return e
	}
	if left.Sub(held).Add(dec(owned.Held)).LessThan(required) {
		return conflict("credit_exhausted", "credit does not cover the next billing window")
	}
	return nil
}

// CancelUnprovisionedInstance is only for a creation proven never dispatched by
// the caller, with guest absence, instance ownership and a live task lease checked
// in the same transaction. It releases held funds through the existing ledger;
// it never settles, refunds, deletes history or treats a running guest as stopped.
func CancelUnprovisionedInstance(ctx context.Context, db *gorm.DB, instance row) error {
	db = db.WithContext(ctx)
	ac, e := account(db, platform.Uint(instance["user_id"]), false)
	if e != nil {
		return e
	}
	ss, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id"), "usage_sessions", "instance_id = ?", instance["id"])
	if e != nil {
		return e
	}
	for _, s := range ss {
		if !pristineCreationSession(s, instance, ac) {
			return conflict("invalid_state", "创建资源的计费记录已有使用证据，预留暂不释放；请管理员核对。")
		}
		var charged int64
		if e = db.Table("usage_charges").Where("session_id = ?", s["id"]).Count(&charged).Error; e != nil {
			return e
		}
		if charged != 0 {
			return conflict("invalid_state", "创建资源已有计费区间，预留暂不释放；请管理员核对。")
		}
		rs, err := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id"), "credit_reservations", "session_id = ?", s["id"])
		if err != nil {
			return err
		}
		for _, reservation := range rs {
			if platform.Uint(reservation["account_id"]) != platform.Uint(ac["id"]) || platform.Text(reservation["status"]) != "held" || !dec(reservation["settled_pt"]).IsZero() || !dec(reservation["released_pt"]).IsZero() {
				return conflict("invalid_state", "计费预留已有结算或释放记录，请管理员核对后处理。")
			}
		}
	}
	t := now()
	for _, s := range ss {
		if e = releaseReservations(db, s, t); e != nil {
			return e
		}
		if e = db.Table("usage_sessions").Where("id = ?", s["id"]).Updates(row{"status": "ended", "ended_at": t, "revision": gorm.Expr("revision+1")}).Error; e != nil {
			return e
		}
	}
	return nil
}

func pristineCreationSession(s, instance, ac row) bool {
	kind, state := platform.Text(s["charge_kind"]), platform.Text(s["metering_state"])
	// Admission starts retained-storage clocks before a guest exists. These may
	// be cancelled only without any metering/charge history and caller absence proof.
	return platform.Uint(s["account_id"]) == platform.Uint(ac["id"]) && platform.Uint(s["user_id"]) == platform.Uint(instance["user_id"]) && platform.Uint(s["node_id"]) == platform.Uint(instance["node_id"]) && platform.Uint(s["instance_id"]) == platform.Uint(instance["id"]) && platform.Text(s["status"]) == "active" && s["ended_at"] == nil && platform.Uint(s["metered_microseconds"]) == 0 && dec(s["cumulative_expected_pt"]).IsZero() && stamp(s["started_at"]).Equal(stamp(s["last_settled_at"])) && ((state == "held" && (kind == "base_compute" || kind == "gpu")) || (state == "running" && kind == "storage"))
}
func InstanceUsage(ctx context.Context, db *gorm.DB, instance row, event string, allocationIDs []uint64) error {
	if !in(event, "disk_reserved", "running", "stopped", "resize_stopped", "deleted", "resize", "resize_reserved", "resize_running_reserved", "gpu_reserved", "gpu_attached", "gpu_released") {
		return invalid("unsupported usage lifecycle event")
	}
	db = db.WithContext(ctx)
	ac, e := account(db, platform.Uint(instance["user_id"]), true)
	if e != nil {
		return e
	}
	p, e := accountPolicy(db, ac)
	if e != nil {
		return e
	}
	t := now()
	iid := platform.Uint(instance["id"])
	if event == "resize_reserved" || event == "resize_running_reserved" {
		ss, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}), "usage_sessions", "instance_id = ? AND status = 'active' AND charge_kind IN ('base_compute','storage')", iid)
		if e != nil {
			return e
		}
		for _, s := range ss {
			// Price only the reservation request at the proposed size. In particular,
			// a running resize must settle the old session at its original snapshot
			// and rates when STOP is confirmed, never retroactively at the new size.
			reservationRate := row{}
			for key, value := range s {
				reservationRate[key] = value
			}
			reservationRate["config_snapshot"] = jsonValue(row{"vcpus": instance["vcpus"], "memory_bytes": sid(instance["memory_bytes"]), "disk_bytes": sid(instance["disk_bytes"])})
			if platform.Text(s["charge_kind"]) == "base_compute" {
				reservationRate["cpu_hourly_pt"] = instance["cpu_hourly_pt"]
				reservationRate["memory_gib_hourly_pt"] = instance["memory_gib_hourly_pt"]
			} else {
				reservationRate["disk_gib_hourly_pt"] = instance["disk_gib_hourly_pt"]
			}
			if e = reserveWindow(db, ac, p, reservationRate, t, true); e != nil {
				return e
			}
		}
		return nil
	}
	if in(event, "stopped", "resize_stopped", "deleted", "resize") {
		ss, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}), "usage_sessions", "instance_id = ? AND status = 'active'", iid)
		if e != nil {
			return e
		}
		for _, s := range ss {
			kind := platform.Text(s["charge_kind"])
			if in(event, "stopped", "resize_stopped") && (kind == "gpu" || (kind == "storage" && platform.Text(p["storage_charge_mode"]) == "retained")) {
				continue
			}
			if event == "resize" && kind == "gpu" {
				continue
			}
			if e = endSession(db, ac, p, s, t); e != nil {
				return e
			}
		}
		if event == "resize_stopped" {
			// STOP settlement closes the old compute snapshot and reserves the
			// replacement held session in one account-locked transaction.
			return beginSession(db, ac, p, instance, "base_compute", nil, "held", t)
		}
		if event != "resize" {
			return nil
		}
	}
	if in(event, "disk_reserved", "running", "resize") {
		storage := platform.Text(p["storage_charge_mode"])
		if storage == "retained" || (storage == "running_only" && (event == "running" || platform.Text(instance["observed_state"]) == "running")) {
			if e = beginSession(db, ac, p, instance, "storage", nil, "running", t); e != nil {
				return e
			}
		}
		state := "held"
		if event == "running" || platform.Text(instance["observed_state"]) == "running" {
			state = "running"
		}
		if e = beginSession(db, ac, p, instance, "base_compute", nil, state, t); e != nil {
			return e
		}
	}
	if in(event, "gpu_reserved", "gpu_attached", "gpu_released") {
		for _, id := range allocationIDs {
			g, e := platform.Get(db, "gpu_allocations", id, true)
			if e != nil {
				return e
			}
			if platform.Uint(g["instance_id"]) != iid {
				return conflict("invalid_state", "GPU allocation belongs to another instance")
			}
			if event == "gpu_released" {
				ss, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}), "usage_sessions", "gpu_allocation_id = ? AND status = 'active'", id)
				if e != nil {
					return e
				}
				for _, s := range ss {
					if e = endSession(db, ac, p, s, t); e != nil {
						return e
					}
				}
			} else {
				state := "held"
				if event == "gpu_attached" {
					state = "running"
				}
				if e = beginSession(db, ac, p, instance, "gpu", g, state, t); e != nil {
					return e
				}
			}
		}
	}
	return nil
}
func beginSession(db *gorm.DB, ac, p, instance row, kind string, gpu row, state string, t time.Time) error {
	q := db.Table("usage_sessions").Clauses(clause.Locking{Strength: "UPDATE"}).Where("instance_id = ? AND charge_kind = ? AND status = 'active'", instance["id"], kind)
	if gpu != nil {
		q = q.Where("gpu_allocation_id = ?", gpu["id"])
	}
	var old row
	e := q.Take(&old).Error
	if e == nil {
		if state == "running" && platform.Text(old["metering_state"]) == "held" {
			if e = releaseReservations(db, old, t); e != nil {
				return e
			}
			if e = db.Table("usage_sessions").Where("id = ?", old["id"]).Updates(row{"metering_state": "running", "started_at": t, "last_settled_at": t, "revision": gorm.Expr("revision + 1")}).Error; e != nil {
				return e
			}
			old["started_at"] = t
			old["last_settled_at"] = t
			old["metering_state"] = "running"
			return reserveWindow(db, ac, p, old, t, false)
		}
		return nil
	}
	if e != gorm.ErrRecordNotFound {
		return e
	}
	s := row{"account_id": ac["id"], "user_id": instance["user_id"], "instance_id": instance["id"], "node_id": instance["node_id"], "charge_kind": kind, "pricing_version_id": instance["pricing_version_id"], "billing_mode": instance["billing_mode"], "config_snapshot": jsonValue(row{"vcpus": instance["vcpus"], "memory_bytes": sid(instance["memory_bytes"]), "disk_bytes": sid(instance["disk_bytes"])}), "cpu_hourly_pt": "0.000000", "memory_gib_hourly_pt": "0.000000", "disk_gib_hourly_pt": "0.000000", "gpu_hourly_pt": "0.000000", "status": "active", "metering_state": state, "started_at": t, "last_settled_at": t}
	switch kind {
	case "base_compute":
		s["cpu_hourly_pt"] = instance["cpu_hourly_pt"]
		s["memory_gib_hourly_pt"] = instance["memory_gib_hourly_pt"]
	case "storage":
		s["disk_gib_hourly_pt"] = instance["disk_gib_hourly_pt"]
	case "gpu":
		s["gpu_allocation_id"] = gpu["id"]
		s["gpu_price_version_id"] = gpu["gpu_price_version_id"]
		s["gpu_hourly_pt"] = gpu["gpu_hourly_pt"]
	}
	s, e = insert(db, "usage_sessions", s)
	if e != nil {
		return e
	}
	return reserveWindow(db, ac, p, s, t, state == "held")
}
func expected(s row, microseconds uint64) decimal.Decimal {
	var cfg row
	_ = json.Unmarshal([]byte(platform.Text(s["config_snapshot"])), &cfg)
	rat := new(big.Rat)
	add := func(value any, mul uint64, den int64) {
		r, ok := new(big.Rat).SetString(platform.Text(value))
		if !ok {
			return
		}
		r.Mul(r, new(big.Rat).SetInt(new(big.Int).SetUint64(mul)))
		r.Quo(r, big.NewRat(den, 1))
		rat.Add(rat, r)
	}
	add(s["cpu_hourly_pt"], platform.Uint(cfg["vcpus"]), 1)
	add(s["memory_gib_hourly_pt"], platform.Uint(cfg["memory_bytes"]), 1073741824)
	add(s["disk_gib_hourly_pt"], platform.Uint(cfg["disk_bytes"]), 1073741824)
	add(s["gpu_hourly_pt"], 1, 1)
	// Points * microseconds / 3600 gives micro-points. Round the cumulative
	// rational, never each independent slice, conserving every six-decimal unit.
	rat.Mul(rat, new(big.Rat).SetInt(new(big.Int).SetUint64(microseconds)))
	rat.Quo(rat, big.NewRat(3600, 1))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(rat.Num(), rat.Denom(), rem)
	if new(big.Int).Lsh(rem, 1).Cmp(rat.Denom()) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return decimal.NewFromBigInt(q, -6)
}
func reserveWindow(db *gorm.DB, ac, p, s row, t time.Time, strict bool) error {
	if platform.Text(s["billing_mode"]) == "admin_exempt" {
		return nil
	}
	need := expected(s, platform.Uint(p["billing_window_seconds"])*1000000)
	if need.IsZero() {
		return nil
	}
	existing, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}), "credit_reservations", "session_id = ? AND status = 'held'", s["id"])
	if e != nil {
		return e
	}
	for _, r := range existing {
		need = need.Sub(dec(r["reserved_pt"]).Sub(dec(r["settled_pt"])).Sub(dec(r["released_pt"])))
	}
	if !need.IsPositive() {
		return nil
	}
	bs, e := buckets(db, platform.Uint(ac["id"]), t)
	if e != nil {
		return e
	}
	total := decimal.Zero
	for _, b := range bs {
		total = total.Add(dec(b["remaining_pt"]).Sub(dec(b["reserved_pt"])))
	}
	if strict && total.LessThan(need) {
		return conflict("credit_exhausted", "credit cannot cover the requested resource window")
	}
	end := t.Add(time.Duration(platform.Uint(p["billing_window_seconds"])) * time.Second)
	key := "session:" + sid(s["id"]) + ":" + strconv.FormatInt(t.UnixMicro(), 10)
	for _, b := range bs {
		v := decimal.Min(need, dec(b["remaining_pt"]).Sub(dec(b["reserved_pt"])))
		if !v.IsPositive() {
			continue
		}
		r, e := insert(db, "credit_reservations", row{"reservation_key": key, "account_id": ac["id"], "session_id": s["id"], "bucket_id": b["id"], "window_from": t, "window_to": end, "reserved_at": t, "expires_at": end, "reserved_pt": money(v), "settled_pt": "0.000000", "released_pt": "0.000000", "status": "held"})
		if e != nil {
			return e
		}
		if e = mutateBucket(db, b, "reserve:"+sid(r["id"]), "reserve", decimal.Zero, v, row{"reservation_id": r["id"]}); e != nil {
			return e
		}
		need = need.Sub(v)
		if need.IsZero() {
			break
		}
	}
	return nil
}
func releaseReservations(db *gorm.DB, s row, t time.Time) error {
	rs, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id"), "credit_reservations", "session_id = ? AND status = 'held'", s["id"])
	if e != nil {
		return e
	}
	for _, r := range rs {
		v := dec(r["reserved_pt"]).Sub(dec(r["settled_pt"])).Sub(dec(r["released_pt"]))
		b, e := platform.Get(db, "credit_buckets", platform.Uint(r["bucket_id"]), true)
		if e != nil {
			return e
		}
		if e = mutateBucket(db, b, "release:"+sid(r["id"]), "release", decimal.Zero, v.Neg(), row{"reservation_id": r["id"]}); e != nil {
			return e
		}
		if e = db.Table("credit_reservations").Where("id = ?", r["id"]).Updates(row{"released_pt": money(dec(r["released_pt"]).Add(v)), "status": "released", "closed_at": t, "revision": gorm.Expr("revision + 1")}).Error; e != nil {
			return e
		}
	}
	return nil
}
func payCharge(db *gorm.DB, ac, s, charge row, t time.Time) error {
	need := dec(charge["expected_pt"]).Sub(dec(charge["settled_pt"]))
	paid := dec(charge["settled_pt"])
	if !need.IsPositive() {
		return nil
	}
	// Held funds remain payable across bucket expiry, but only by their owning session.
	rs, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id"), "credit_reservations", "session_id = ? AND status = 'held'", s["id"])
	if e != nil {
		return e
	}
	for _, r := range rs {
		v := decimal.Min(need, dec(r["reserved_pt"]).Sub(dec(r["settled_pt"])).Sub(dec(r["released_pt"])))
		if !v.IsPositive() {
			continue
		}
		b, e := platform.Get(db, "credit_buckets", platform.Uint(r["bucket_id"]), true)
		if e != nil {
			return e
		}
		if e = mutateBucket(db, b, "settle:"+sid(charge["id"])+":reservation:"+sid(r["id"]), "settle", v.Neg(), v.Neg(), row{"charge_id": charge["id"], "reservation_id": r["id"]}); e != nil {
			return e
		}
		settled := dec(r["settled_pt"]).Add(v)
		changes := row{"settled_pt": money(settled), "revision": gorm.Expr("revision + 1")}
		if settled.Add(dec(r["released_pt"])).Equal(dec(r["reserved_pt"])) {
			changes["status"] = "settled"
			changes["closed_at"] = t
		}
		if e = db.Table("credit_reservations").Where("id = ?", r["id"]).Updates(changes).Error; e != nil {
			return e
		}
		need = need.Sub(v)
		paid = paid.Add(v)
		if need.IsZero() {
			break
		}
	}
	bs, e := buckets(db, platform.Uint(ac["id"]), t)
	if e != nil {
		return e
	}
	for _, b := range bs {
		if need.IsZero() {
			break
		}
		v := decimal.Min(need, dec(b["remaining_pt"]).Sub(dec(b["reserved_pt"])))
		if !v.IsPositive() {
			continue
		}
		key, e := randomKey()
		if e != nil {
			return e
		}
		if e = mutateBucket(db, b, "settle:"+sid(charge["id"])+":"+key, "settle", v.Neg(), decimal.Zero, row{"charge_id": charge["id"]}); e != nil {
			return e
		}
		need = need.Sub(v)
		paid = paid.Add(v)
	}
	status := "settled"
	var settledAt any = t
	if need.IsPositive() {
		status = "insufficient"
		settledAt = nil
	}
	charge["settled_pt"] = money(paid)
	charge["status"] = status
	return db.Table("usage_charges").Where("id = ?", charge["id"]).Updates(row{"settled_pt": money(paid), "status": status, "settled_at": settledAt, "revision": gorm.Expr("revision + 1")}).Error
}
func settleSession(db *gorm.DB, ac, p, s row, t time.Time) error {
	from := stamp(s["last_settled_at"])
	if !t.After(from) || platform.Text(s["metering_state"]) == "held" {
		return nil
	}
	duration := uint64(t.Sub(from).Microseconds())
	metered := platform.Uint(s["metered_microseconds"])
	billable := true
	deferred := false
	var event row
	er := db.Table("credit_enforcements").Where("account_id = ? AND status IN ('grace','stopping','unknown')", ac["id"]).Take(&event).Error
	if er != nil && er != gorm.ErrRecordNotFound {
		return er
	}
	if er == nil {
		var snapshot row
		if e := json.Unmarshal([]byte(platform.Text(event["policy_snapshot"])), &snapshot); e != nil {
			return e
		}
		if platform.Text(snapshot["grace_charge_mode"]) == "waived" && !from.Before(stamp(event["exhausted_at"])) {
			billable = false
		}
	}
	if platform.Text(s["billing_mode"]) == "admin_exempt" {
		billable = false
	}
	if platform.Text(p["offline_charge_mode"]) == "defer" {
		var n int64
		if e := db.Table("nodes").Where("id = ? AND health <> 'online'", s["node_id"]).Count(&n).Error; e != nil {
			return e
		}
		deferred = n > 0
	}
	total := dec(s["cumulative_expected_pt"])
	value := decimal.Zero
	if billable {
		metered += duration
		next := expected(s, metered)
		value = next.Sub(total)
		total = next
	}
	status := "pending"
	var paidAt any
	if value.IsZero() {
		status = "settled"
		paidAt = t
	}
	charge, e := insert(db, "usage_charges", row{"session_id": s["id"], "account_id": ac["id"], "from_at": from, "to_at": t, "expected_pt": money(value), "settled_pt": "0.000000", "status": status, "settled_at": paidAt, "calculation_snapshot": jsonValue(row{"metered_microseconds": strconv.FormatUint(metered, 10), "cumulative_expected_pt": money(total), "waived": !billable, "offline_deferred": deferred, "policy_version_id": sid(p["policy_version_id"])})})
	if e != nil {
		return e
	}
	if !deferred && !value.IsZero() {
		if e = payCharge(db, ac, s, charge, t); e != nil {
			return e
		}
	}
	if e = releaseReservations(db, s, t); e != nil {
		return e
	}
	if e = db.Table("usage_sessions").Where("id = ?", s["id"]).Updates(row{"last_settled_at": t, "metered_microseconds": metered, "cumulative_expected_pt": money(total), "revision": gorm.Expr("revision + 1")}).Error; e != nil {
		return e
	}
	s["last_settled_at"] = t
	s["metered_microseconds"] = metered
	s["cumulative_expected_pt"] = money(total)
	return nil
}
func endSession(db *gorm.DB, ac, p, s row, t time.Time) error {
	if e := settleSession(db, ac, p, s, t); e != nil {
		return e
	}
	if e := releaseReservations(db, s, t); e != nil {
		return e
	}
	return db.Table("usage_sessions").Where("id = ?", s["id"]).Updates(row{"status": "ended", "ended_at": t, "revision": gorm.Expr("revision + 1")}).Error
}
func cycleAt(p row, t time.Time) (time.Time, error) {
	loc, e := time.LoadLocation(platform.Text(p["timezone"]))
	if e != nil {
		return time.Time{}, e
	}
	clock, e := time.Parse("15:04:05", platform.Text(p["grant_time"]))
	if e != nil {
		return time.Time{}, e
	}
	local := t.In(loc)
	cycle := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), clock.Second(), 0, loc)
	if cycle.After(local) {
		cycle = cycle.AddDate(0, 0, -1)
	}
	return cycle, nil
}
func dailyGrants(ctx context.Context, a *platform.App) error {
	_, e := activePolicy(a.DB.WithContext(ctx), "credit")
	if e != nil {
		return e
	}
	var users []uint64
	if e = a.DB.WithContext(ctx).Table("users").Where("status = 'active'").Order("id").Pluck("id", &users).Error; e != nil {
		return e
	}
	for _, uid := range users {
		if e = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
			ac, e := account(db, uid, true)
			if e != nil {
				return e
			}
			if platform.Text(ac["status"]) != "active" {
				return nil
			}
			cfg, e := accountPolicy(db, ac)
			if e != nil {
				return e
			}
			last, e := cycleAt(cfg, now())
			if e != nil {
				return e
			}
			start := stamp(ac["grant_cursor_at"])
			if start.IsZero() {
				start, e = cycleAt(cfg, stamp(ac["created_at"]))
				if e != nil {
					return e
				}
			} else {
				start = start.In(last.Location()).AddDate(0, 0, 1)
			}
			for cycle := start; !cycle.After(last); cycle = cycle.AddDate(0, 0, 1) {
				date := cycle.Format("2006-01-02")
				var n int64
				if e = db.Table("credit_daily_grants").Where("account_id = ? AND business_date = ?", ac["id"], date).Count(&n).Error; e != nil {
					return e
				}
				if n == 0 {
					end := cycle.Add(time.Duration(platform.Uint(cfg["daily_validity_seconds"])) * time.Second)
					record := row{"account_id": ac["id"], "business_date": date, "policy_version_id": ac["credit_policy_version_id"], "valid_from": cycle.In(zone), "expires_at": end.In(zone)}
					b, e := grant(db, ac, "daily", "daily:"+date, dec(cfg["daily_allowance_pt"]), cycle.In(zone), &end, 0)
					if e != nil {
						return e
					}
					record["bucket_id"] = b["id"]
					if e = db.Table("credit_daily_grants").Create(&record).Error; e != nil {
						return e
					}
				}
				if e = db.Table("credit_accounts").Where("id = ?", ac["id"]).Updates(row{"grant_cursor_at": cycle.In(zone)}).Error; e != nil {
					return e
				}
			}
			return restore(db, ac, now())
		}); e != nil {
			return e
		}
	}
	return nil
}
func expireBuckets(db *gorm.DB, ac row, t time.Time) error {
	bs, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id"), "credit_buckets", "account_id = ? AND expires_at <= ? AND remaining_pt > reserved_pt", ac["id"], t)
	if e != nil {
		return e
	}
	for _, b := range bs {
		v := dec(b["remaining_pt"]).Sub(dec(b["reserved_pt"]))
		key, e := randomKey()
		if e != nil {
			return e
		}
		if e = mutateBucket(db, b, "expire:"+key, "expire", v.Neg(), decimal.Zero, nil); e != nil {
			return e
		}
	}
	return nil
}
func settleAll(ctx context.Context, a *platform.App) error {
	var ids []uint64
	if e := a.DB.WithContext(ctx).Table("credit_accounts").Where("status <> 'closed'").Order("id").Pluck("id", &ids).Error; e != nil {
		return e
	}
	for _, id := range ids {
		if e := a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
			ac, e := platform.Get(db, "credit_accounts", id, true)
			if e != nil {
				return e
			}
			p, e := accountPolicy(db, ac)
			if e != nil {
				return e
			}
			t := now()
			ss, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id"), "usage_sessions", "account_id = ? AND status = 'active'", id)
			if e != nil {
				return e
			}
			for _, s := range ss {
				if e = settleSession(db, ac, p, s, t); e != nil {
					return e
				}
			}
			if e = expireBuckets(db, ac, t); e != nil {
				return e
			}
			if e = restore(db, ac, t); e != nil {
				return e
			}
			if e = detectExhaustion(db, ac, p, t); e != nil {
				return e
			}
			for _, s := range ss {
				if e = reserveWindow(db, ac, p, s, t, false); e != nil {
					return e
				}
			}
			return nil
		}); e != nil {
			return e
		}
	}
	return nil
}
func restore(db *gorm.DB, ac row, t time.Time) error {
	// Pay previously accrued grace debt first; explicitly deferred offline intervals
	// stay pending until an administrator can establish their actual ownership time.
	charges, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id"), "usage_charges", "account_id = ? AND status = 'insufficient'", ac["id"])
	if e != nil {
		return e
	}
	for _, c := range charges {
		s, e := platform.Get(db, "usage_sessions", platform.Uint(c["session_id"]), true)
		if e != nil {
			return e
		}
		if e = payCharge(db, ac, s, c, t); e != nil {
			return e
		}
	}
	var event row
	er := db.Table("credit_enforcements").Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id = ? AND status IN ('grace','stopping','unknown')", ac["id"]).Take(&event).Error
	if er == gorm.ErrRecordNotFound {
		return nil
	}
	if er != nil {
		return er
	}
	var p row
	if e = json.Unmarshal([]byte(platform.Text(event["policy_snapshot"])), &p); e != nil {
		return e
	}
	if platform.Text(p["restore_action"]) != "cancel_pending_stop" {
		return nil
	}
	left, _, e := balances(db, platform.Uint(ac["id"]))
	if e != nil {
		return e
	}
	if !left.IsPositive() {
		return nil
	}
	var debt int64
	if e = db.Table("usage_charges").Where("account_id = ? AND status = 'insufficient'", ac["id"]).Count(&debt).Error; e != nil {
		return e
	}
	if debt > 0 {
		return nil
	}
	sessions, e := rows(db, "usage_sessions", "account_id = ? AND status = 'active' AND billing_mode = 'credit'", ac["id"])
	if e != nil {
		return e
	}
	need := decimal.Zero
	for _, s := range sessions {
		need = need.Add(expected(s, platform.Uint(p["billing_window_seconds"])*1000000))
	}
	if left.LessThan(need) {
		return nil
	}
	if e = db.Table("credit_enforcements").Where("id = ?", event["id"]).Updates(row{"status": "cancelled", "revision": gorm.Expr("revision + 1")}).Error; e != nil {
		return e
	}
	return notify(db, platform.Uint(ac["user_id"]), "credit_restored", "Credit has been restored. Pending credit shutdown is cancelled; stopped instances are not restarted automatically.", "credit_enforcement", platform.Uint(event["id"]))
}
