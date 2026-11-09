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
const settlementChunkSize = 32

func AdmitInstance(ctx context.Context, db *gorm.DB, instance row, action string) error {
	if platform.Text(instance["billing_mode"]) == "admin_exempt" {
		return nil
	}
	ac, e := account(db.WithContext(ctx), platform.Uint(instance["user_id"]))
	if e != nil {
		return e
	}
	if failed, err := instanceCreationFailed(db.WithContext(ctx), instance); err != nil {
		return err
	} else if failed {
		return conflict("invalid_state", "failed creations cannot admit paid resources")
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
	p, e := instanceCreditPolicy(db, instance)
	if e != nil {
		return e
	}
	left, held, e := balances(db, platform.Uint(ac["id"]))
	if e != nil {
		return e
	}
	debt, e := outstandingDebt(db, platform.Uint(ac["id"]))
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
	if left.Sub(held).Sub(debt).Add(dec(owned.Held)).LessThan(required) {
		return conflict("credit_exhausted", "credit does not cover the next billing window")
	}
	return nil
}

func InstanceUsage(ctx context.Context, db *gorm.DB, instance row, event string, allocationIDs []uint64) error {
	return instanceUsageAt(ctx, db, instance, event, allocationIDs, now())
}

func instanceUsageAt(ctx context.Context, db *gorm.DB, instance row, event string, allocationIDs []uint64, t time.Time) error {
	if !in(event, "disk_reserved", "running", "stopped", "resize_stopped", "deleted", "resize", "resize_reserved", "resize_running_reserved", "gpu_reserved", "gpu_attached", "gpu_released", "gpu_reservation_cancelled") {
		return invalid("unsupported usage lifecycle event")
	}
	db = db.WithContext(ctx)
	ac, e := usageAccount(db, instance)
	if e != nil || ac == nil {
		return e
	}
	if failed, err := instanceCreationFailed(db, instance); err != nil {
		return err
	} else if failed {
		return nil
	}
	p, e := instanceCreditPolicy(db, instance)
	if e != nil {
		return e
	}
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
			if event == "resize" && kind == "gpu" {
				continue
			}
			if e = endSession(db, ac, p, s, t); e != nil {
				return e
			}
		}
		if event != "resize" {
			return nil
		}
	}
	if in(event, "disk_reserved", "running", "resize") {
		// Admission holds funds, but neither a disk reservation nor a resize
		// request proves that the guest is running.
		state := "held"
		if event == "running" {
			state = "running"
		}
		if platform.Text(p["storage_charge_mode"]) != "free" {
			if e = beginSession(db, ac, p, instance, "storage", nil, state, t); e != nil {
				return e
			}
		}
		if e = beginSession(db, ac, p, instance, "base_compute", nil, state, t); e != nil {
			return e
		}
		if event == "running" {
			gs, err := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}), "gpu_allocations", "instance_id = ? AND status = 'active' AND configured = TRUE", iid)
			if err != nil {
				return err
			}
			for _, g := range gs {
				if e = beginSession(db, ac, p, instance, "gpu", g, "running", t); e != nil {
					return e
				}
			}
		}
	}
	if in(event, "gpu_reserved", "gpu_attached", "gpu_released", "gpu_reservation_cancelled") {
		for _, id := range allocationIDs {
			g, e := platform.Get(db, "gpu_allocations", id, true)
			if e != nil {
				return e
			}
			if platform.Uint(g["instance_id"]) != iid {
				return conflict("invalid_state", "GPU allocation belongs to another instance")
			}
			if in(event, "gpu_released", "gpu_reservation_cancelled") {
				q := db.Clauses(clause.Locking{Strength: "UPDATE"})
				if event == "gpu_reservation_cancelled" {
					q = q.Where("metering_state = 'held'")
				}
				ss, e := rows(q, "usage_sessions", "gpu_allocation_id = ? AND status = 'active'", id)
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
					current, err := platform.Get(db.Clauses(clause.Locking{Strength: "SHARE"}), "instances", iid, false)
					if err != nil {
						return err
					}
					if !freshUsageObservation(current, t) || platform.Text(current["observed_state"]) != "running" {
						continue
					}
					if platform.Text(g["status"]) != "active" || !platform.Bool(g["configured"]) {
						continue
					}
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
	if failed, err := instanceCreationFailed(db, instance); err != nil {
		return err
	} else if failed {
		return nil
	}
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
	if failed, err := sessionCreationFailed(db, s); err != nil {
		return err
	} else if failed {
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
	q := db
	if !strict {
		q = db.Limit(settlementChunkSize)
	}
	bs, e := buckets(q, platform.Uint(ac["id"]), t)
	if e != nil {
		return e
	}
	total := decimal.Zero
	for _, b := range bs {
		total = total.Add(dec(b["remaining_pt"]).Sub(dec(b["reserved_pt"])))
	}
	debt, e := outstandingDebt(db, platform.Uint(ac["id"]))
	if e != nil {
		return e
	}
	total = decimal.Max(decimal.Zero, total.Sub(debt))
	if strict && total.LessThan(need) {
		return conflict("credit_exhausted", "credit cannot cover the requested resource window")
	}
	need = decimal.Min(need, total)
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
	// A deferred chunk must not release the old owner's funds for new usage,
	// especially once the underlying bucket has expired.
	var unpaid int64
	if e := db.Clauses(clause.Locking{Strength: "UPDATE"}).Table("usage_charges").Where("session_id=? AND status IN ('pending','insufficient') AND expected_pt>settled_pt", s["id"]).Count(&unpaid).Error; e != nil {
		return e
	}
	if unpaid > 0 {
		return nil
	}
	rs, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id").Limit(settlementChunkSize), "credit_reservations", "session_id = ? AND status = 'held'", s["id"])
	if e != nil {
		return e
	}
	for _, r := range rs {
		if e = releaseReservation(db, r, t); e != nil {
			return e
		}
	}
	return nil
}

func releaseReservation(db *gorm.DB, r row, t time.Time) error {
	v := dec(r["reserved_pt"]).Sub(dec(r["settled_pt"])).Sub(dec(r["released_pt"]))
	b, e := platform.Get(db, "credit_buckets", platform.Uint(r["bucket_id"]), true)
	if e != nil {
		return e
	}
	if e = mutateBucket(db, b, "release:"+sid(r["id"]), "release", decimal.Zero, v.Neg(), row{"reservation_id": r["id"]}); e != nil {
		return e
	}
	if expiry := stamp(b["expires_at"]); !expiry.IsZero() && !expiry.After(t) && v.IsPositive() {
		if e = mutateBucket(db, b, "release-expire:"+sid(r["id"]), "expire", v.Neg(), decimal.Zero, row{"reservation_id": r["id"]}); e != nil {
			return e
		}
	}
	return db.Table("credit_reservations").Where("id = ?", r["id"]).Updates(row{"released_pt": money(dec(r["released_pt"]).Add(v)), "status": "released", "closed_at": t, "revision": gorm.Expr("revision+1")}).Error
}

func releaseEndedReservations(db *gorm.DB, ac row, t time.Time) error {
	rs, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("r.id").Limit(settlementChunkSize), "credit_reservations r", "r.account_id=? AND r.status='held' AND EXISTS (SELECT 1 FROM usage_sessions s WHERE s.id=r.session_id AND s.status='ended') AND NOT EXISTS (SELECT 1 FROM usage_charges c WHERE c.session_id=r.session_id AND c.status IN ('pending','insufficient') AND c.expected_pt>c.settled_pt)", ac["id"])
	if e != nil {
		return e
	}
	for _, r := range rs {
		if e = releaseReservation(db, r, t); e != nil {
			return e
		}
	}
	return nil
}

// chargeFunds is confined to one account-locked transaction. It caches locked
// payment sources so a debt backlog does not repeat the same SELECTs per charge.
type chargeFunds struct {
	available    []row
	buckets      map[uint64]row
	reservations map[uint64][]row
	free         decimal.Decimal
	held         decimal.Decimal
}

func loadChargeFunds(db *gorm.DB, ac row, t time.Time, sessionID uint64) (*chargeFunds, error) {
	return loadChargeFundsForSessions(db, ac, t, []uint64{sessionID})
}

func loadChargeFundsForSessions(db *gorm.DB, ac row, t time.Time, sessionIDs []uint64) (*chargeFunds, error) {
	q := db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id").Limit(settlementChunkSize)
	if len(sessionIDs) > 0 && sessionIDs[0] != 0 {
		q = q.Where("session_id IN ?", sessionIDs)
	}
	rs, e := rows(q, "credit_reservations", "account_id = ? AND status = 'held' AND reserved_pt>settled_pt+released_pt", ac["id"])
	if e != nil {
		return nil, e
	}
	bs, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("expires_at IS NULL, expires_at, id").Limit(settlementChunkSize), "credit_buckets", "account_id = ? AND valid_from <= ? AND (expires_at IS NULL OR expires_at > ?) AND remaining_pt > reserved_pt", ac["id"], t, t)
	if e != nil {
		return nil, e
	}
	funds := &chargeFunds{available: bs}
	for _, b := range bs {
		funds.free = funds.free.Add(dec(b["remaining_pt"]).Sub(dec(b["reserved_pt"])))
	}
	if len(rs) > 0 {
		funds.buckets = make(map[uint64]row, len(bs))
		funds.reservations = make(map[uint64][]row)
		for _, b := range bs {
			funds.buckets[platform.Uint(b["id"])] = b
		}
	}
	for _, r := range rs {
		value := dec(r["reserved_pt"]).Sub(dec(r["settled_pt"])).Sub(dec(r["released_pt"]))
		if !value.IsPositive() {
			continue
		}
		bid := platform.Uint(r["bucket_id"])
		if funds.buckets[bid] == nil {
			b, err := platform.Get(db, "credit_buckets", bid, true)
			if err != nil {
				return nil, err
			}
			funds.buckets[bid] = b
		}
		sid := platform.Uint(r["session_id"])
		funds.reservations[sid] = append(funds.reservations[sid], r)
		funds.held = funds.held.Add(value)
	}
	return funds, nil
}

func (f *chargeFunds) canPay(sessionID uint64) bool {
	if f.free.IsPositive() {
		return true
	}
	for _, r := range f.reservations[sessionID] {
		if dec(r["reserved_pt"]).GreaterThan(dec(r["settled_pt"]).Add(dec(r["released_pt"]))) {
			return true
		}
	}
	return false
}

func payCharge(db *gorm.DB, ac, s, charge row, t time.Time) error {
	if failed, err := sessionCreationFailed(db, s); err != nil {
		return err
	} else if failed || platform.Text(charge["status"]) == "cancelled" {
		return nil
	}
	if !dec(charge["expected_pt"]).GreaterThan(dec(charge["settled_pt"])) {
		return nil
	}
	funds, e := loadChargeFunds(db, ac, t, platform.Uint(s["id"]))
	if e != nil {
		return e
	}
	// Only funds exceeding older debt are available to a new interval. Held
	// sources stay exclusive to their owner and its oldest unpaid interval.
	var prior struct{ Amount string }
	if e = db.Clauses(clause.Locking{Strength: "UPDATE"}).Table("usage_charges c").Select("COALESCE(SUM(c.expected_pt-c.settled_pt),0) amount").Where("c.account_id=? AND c.status='insufficient' AND c.id<? AND "+chargeNotFailedSQL, ac["id"], charge["id"]).Scan(&prior).Error; e != nil {
		return e
	}
	funds.free = decimal.Max(decimal.Zero, funds.free.Sub(dec(prior.Amount)))
	var ownedDebt int64
	if e = db.Clauses(clause.Locking{Strength: "UPDATE"}).Table("usage_charges").Where("session_id=? AND status='insufficient' AND id<?", s["id"], charge["id"]).Count(&ownedDebt).Error; e != nil {
		return e
	}
	if ownedDebt > 0 {
		delete(funds.reservations, platform.Uint(s["id"]))
	}
	return funds.pay(db, ac, s, charge, t)
}

func (f *chargeFunds) pay(db *gorm.DB, ac, s, charge row, t time.Time) error {
	if failed, err := sessionCreationFailed(db, s); err != nil {
		return err
	} else if failed || platform.Text(charge["status"]) == "cancelled" {
		return nil
	}
	paid := dec(charge["settled_pt"])
	previousPaid := paid
	need := dec(charge["expected_pt"]).Sub(paid)
	if !need.IsPositive() {
		return nil
	}
	// Held funds survive expiry and remain exclusive to the owning session.
	for _, r := range f.reservations[platform.Uint(s["id"])] {
		v := decimal.Min(need, dec(r["reserved_pt"]).Sub(dec(r["settled_pt"])).Sub(dec(r["released_pt"])))
		if !v.IsPositive() {
			continue
		}
		b := f.buckets[platform.Uint(r["bucket_id"])]
		if e := mutateBucket(db, b, "settle:"+sid(charge["id"])+":reservation:"+sid(r["id"]), "settle", v.Neg(), v.Neg(), row{"charge_id": charge["id"], "reservation_id": r["id"]}); e != nil {
			return e
		}
		settled := dec(r["settled_pt"]).Add(v)
		changes := row{"settled_pt": money(settled), "revision": gorm.Expr("revision + 1")}
		if settled.Add(dec(r["released_pt"])).Equal(dec(r["reserved_pt"])) {
			changes["status"] = "settled"
			changes["closed_at"] = t
		}
		if e := db.Table("credit_reservations").Where("id = ?", r["id"]).Updates(changes).Error; e != nil {
			return e
		}
		r["settled_pt"] = money(settled)
		f.held = f.held.Sub(v)
		need = need.Sub(v)
		paid = paid.Add(v)
		if need.IsZero() {
			break
		}
	}
	for _, b := range f.available {
		if need.IsZero() {
			break
		}
		v := decimal.Min(need, decimal.Min(f.free, dec(b["remaining_pt"]).Sub(dec(b["reserved_pt"]))))
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
		f.free = f.free.Sub(v)
		need = need.Sub(v)
		paid = paid.Add(v)
	}
	status := "settled"
	var settledAt any = t
	if need.IsPositive() {
		status = "insufficient"
		settledAt = nil
	}
	if paid.Equal(previousPaid) && platform.Text(charge["status"]) == status && settledAt == nil && charge["settled_at"] == nil {
		return nil
	}
	charge["settled_pt"] = money(paid)
	charge["status"] = status
	if e := db.Table("usage_charges").Where("id = ?", charge["id"]).Updates(row{"settled_pt": money(paid), "status": status, "settled_at": settledAt, "revision": gorm.Expr("revision + 1")}).Error; e != nil {
		return e
	}
	return refreshSettlementPending(db, platform.Uint(ac["id"]), t)
}
func settleSession(db *gorm.DB, ac, p, s row, t time.Time) error {
	if failed, err := sessionCreationFailed(db, s); err != nil {
		return err
	} else if failed {
		return nil
	}
	from := stamp(s["last_settled_at"])
	if !t.After(from) || platform.Text(s["metering_state"]) == "held" {
		return nil
	}
	duration := uint64(t.Sub(from).Microseconds())
	metered := platform.Uint(s["metered_microseconds"])
	billable := true
	deferred := false
	var event row
	er := db.Table("credit_enforcements e").Select("e.*").Joins("JOIN credit_enforcement_instances m ON m.enforcement_id=e.id").Where("e.account_id = ? AND m.instance_id = ? AND e.status IN ('grace','stopping','unknown')", ac["id"], s["instance_id"]).Take(&event).Error
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
	if e = applyBillingReportCharges(db, platform.Uint(ac["id"]), []uint64{platform.Uint(charge["id"])}, false); e != nil {
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
	// Never rewrite or refund an interval that was settled before cutover.
	if last := stamp(s["last_settled_at"]); last.After(t) {
		t = last
	}
	if e := settleSession(db, ac, p, s, t); e != nil {
		return e
	}
	if e := releaseReservations(db, s, t); e != nil {
		return e
	}
	return db.Table("usage_sessions").Where("id = ?", s["id"]).Updates(row{"status": "ended", "ended_at": t, "revision": gorm.Expr("revision + 1")}).Error
}
func cycleAt(p row, t time.Time) (time.Time, error) {
	if platform.Text(p["timezone"]) == "" {
		return time.Time{}, invalid("daily policy timezone is required")
	}
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
		cycle = time.Date(local.Year(), local.Month(), local.Day()-1, clock.Hour(), clock.Minute(), clock.Second(), 0, loc)
	}
	return cycle, nil
}

// The caller holds the account lock (or has just inserted it). Both registration
// and scheduled grants consume the same business date, bucket key and cursor.
func grantDailyCycle(db *gorm.DB, ac, cfg row, cycle, validFrom time.Time) error {
	date := cycle.Format("2006-01-02")
	var n int64
	if e := db.Table("credit_daily_grants").Where("account_id = ? AND business_date = ?", ac["id"], date).Count(&n).Error; e != nil {
		return e
	}
	if n == 0 {
		end, e := dailyExpiry(cfg, cycle, validFrom)
		if e != nil {
			return e
		}
		b, e := grant(db, ac, "daily", "daily:"+date, dec(cfg["daily_allowance_pt"]), validFrom.In(zone), &end, 0)
		if e != nil {
			return e
		}
		record := row{"account_id": ac["id"], "business_date": date, "policy_version_id": ac["credit_policy_version_id"], "bucket_id": b["id"], "valid_from": validFrom.In(zone), "expires_at": end.In(zone), "reset_capped": true}
		if e = db.Table("credit_daily_grants").Create(&record).Error; e != nil {
			return e
		}
	}
	return db.Table("credit_accounts").Where("id = ?", ac["id"]).Updates(row{"grant_cursor_at": cycle.In(zone)}).Error
}

func dailyGrants(ctx context.Context, a *platform.App) error {
	var users []uint64
	if e := a.DB.WithContext(ctx).Table("credit_accounts c").Joins("JOIN users u ON u.id=c.user_id").Where("u.status = 'active' AND c.status = 'active'").Order("c.id").Pluck("c.user_id", &users).Error; e != nil {
		return e
	}
	for _, uid := range users {
		if e := a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
			ac, e := account(db, uid)
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
			// Closed periods never become a spendable catch-up balance. An account
			// created administratively after this boundary waits for the next one;
			// registration has already consumed its current business date.
			cursor := stamp(ac["grant_cursor_at"])
			if (cursor.IsZero() && last.Before(stamp(ac["created_at"]))) || !cursor.Before(last) {
				return nil
			}
			if e = expireBuckets(db, ac, now()); e != nil {
				return e
			}
			return grantDailyCycle(db, ac, cfg, last, last)
		}); e != nil {
			return e
		}
	}
	return nil
}
func expireBuckets(db *gorm.DB, ac row, t time.Time) error {
	bs, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id").Limit(settlementChunkSize), "credit_buckets", "account_id = ? AND expires_at <= ? AND remaining_pt > reserved_pt", ac["id"], t)
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

// Only recent, non-future observations may change metering. A node going
// offline is not evidence that its guests stopped.
func freshUsageObservation(instance row, at time.Time) bool {
	observed := stamp(instance["observed_at"])
	return !observed.IsZero() && !observed.After(at) && at.Sub(observed) <= time.Minute
}

func reconcileInstanceUsage(ctx context.Context, db *gorm.DB, instance row, t time.Time) error {
	if instance["deleted_at"] != nil || instance["creation_failed_at"] != nil || !freshUsageObservation(instance, t) {
		return nil
	}
	state := platform.Text(instance["observed_state"])
	if state != "running" && state != "stopped" {
		return nil
	}
	var online int64
	if err := db.Table("nodes").Where("id=? AND health='online'", instance["node_id"]).Count(&online).Error; err != nil {
		return err
	}
	if online == 0 {
		return nil
	}
	observed := stamp(instance["observed_at"])
	if state == "running" {
		var last struct{ EndedAt *time.Time }
		if err := db.Table("usage_sessions").Select("MAX(ended_at) ended_at").Where("instance_id=?", instance["id"]).Scan(&last).Error; err != nil {
			return err
		}
		if last.EndedAt != nil && !observed.After(*last.EndedAt) {
			return nil
		}
		return instanceUsageAt(ctx, db, instance, "running", nil, observed)
	}
	ac, err := usageAccount(db, instance)
	if err != nil || ac == nil {
		return err
	}
	p, err := instanceCreditPolicy(db, instance)
	if err != nil {
		return err
	}
	sessions, err := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}), "usage_sessions", "instance_id=? AND status='active' AND metering_state<>'held'", instance["id"])
	if err != nil {
		return err
	}
	for _, session := range sessions {
		if observed.Before(stamp(session["started_at"])) {
			continue
		}
		if err = endSession(db, ac, p, session, observed); err != nil {
			return err
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
		var instances []uint64
		if e := a.DB.WithContext(ctx).Table("instances").Where("user_id=(SELECT user_id FROM credit_accounts WHERE id=?) AND deleted_at IS NULL AND creation_failed_at IS NULL", id).Order("id").Pluck("id", &instances).Error; e != nil {
			return e
		}
		for _, instanceID := range instances {
			if err := refreshUsageObservation(ctx, a, instanceID); err != nil {
				return err
			}
			if e := a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
				if _, err := platform.Get(db, "credit_accounts", id, true); err != nil {
					return err
				}
				instance, err := platform.Get(db, "instances", instanceID, true)
				if err != nil {
					return err
				}
				return reconcileInstanceUsage(ctx, db, instance, now())
			}); e != nil {
				return e
			}
		}
		var sessions []uint64
		if e := a.DB.WithContext(ctx).Table("usage_sessions").Where("account_id=? AND status='active'", id).Order("id").Pluck("id", &sessions).Error; e != nil {
			return e
		}
		// Commit each live interval independently. Neither thousands of active
		// sessions nor historical charges extend one account-lock transaction.
		for _, sessionID := range sessions {
			if e := a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
				ac, err := platform.Get(db, "credit_accounts", id, true)
				if err != nil {
					return err
				}
				s, err := platform.Get(db, "usage_sessions", sessionID, true)
				if err != nil {
					return err
				}
				if platform.Text(s["status"]) != "active" {
					return nil
				}
				if failed, err := sessionCreationFailed(db, s); err != nil {
					return err
				} else if failed {
					return nil
				}
				p, err := sessionCreditPolicy(db, s)
				if err != nil {
					return err
				}
				t := now()
				if platform.Text(s["metering_state"]) == "held" {
					return nil
				}
				instance, err := platform.Get(db.Clauses(clause.Locking{Strength: "SHARE"}), "instances", platform.Uint(s["instance_id"]), false)
				if err != nil {
					return err
				}
				if freshUsageObservation(instance, t) && platform.Text(instance["observed_state"]) == "stopped" && !stamp(instance["observed_at"]).Before(stamp(s["started_at"])) {
					var online int64
					if err = db.Table("nodes").Where("id=? AND health='online'", instance["node_id"]).Count(&online).Error; err != nil {
						return err
					}
					if online > 0 {
						return endSession(db, ac, p, s, stamp(instance["observed_at"]))
					}
				}
				if err = settleSession(db, ac, p, s, t); err != nil {
					return err
				}
				return reserveWindow(db, ac, p, s, t, false)
			}); e != nil {
				return e
			}
		}
		if e := a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
			ac, err := platform.Get(db, "credit_accounts", id, true)
			if err != nil {
				return err
			}
			t := now()
			if err = settleDeferredCharges(db, ac, t); err != nil {
				return err
			}
			if err = expireBuckets(db, ac, t); err != nil {
				return err
			}
			if err = releaseEndedReservations(db, ac, t); err != nil {
				return err
			}
			if err = restore(db, ac, t); err != nil {
				return err
			}
			return detectExhaustion(db, ac, t)
		}); e != nil {
			return e
		}
	}
	return nil
}

// Offline deferral keeps the original reservation owner, even after STOP or
// bucket expiry. Once its node is online, make the normal payment attempt.
func settleDeferredCharges(db *gorm.DB, ac row, t time.Time) error {
	charges, err := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("c.id").Limit(settlementChunkSize), "usage_charges c", "c.account_id=? AND c.status='pending' AND JSON_EXTRACT(c.calculation_snapshot,'$.offline_deferred')=true AND EXISTS (SELECT 1 FROM usage_sessions s JOIN nodes n ON n.id=s.node_id WHERE s.id=c.session_id AND n.health='online')", ac["id"])
	if err != nil {
		return err
	}
	for _, charge := range charges {
		if err = payCharge(db, ac, row{"id": charge["session_id"]}, charge, t); err != nil {
			return err
		}
	}
	return nil
}

// Every restore caller, including enforcement gates, pays only one chunk.
func restore(db *gorm.DB, ac row, t time.Time) error {
	charges, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("c.id").Limit(settlementChunkSize), "usage_charges c", "c.account_id=? AND c.status='insufficient' AND "+payableChargeSQL, ac["id"], t, t)
	if e != nil {
		return e
	}
	if len(charges) > 0 {
		ids := make([]uint64, 0, len(charges))
		for _, c := range charges {
			ids = append(ids, platform.Uint(c["session_id"]))
		}
		funds, err := loadChargeFundsForSessions(db, ac, t, ids)
		if err != nil {
			return err
		}
		for _, c := range charges {
			if !funds.canPay(platform.Uint(c["session_id"])) {
				continue
			}
			if err = funds.pay(db, ac, row{"id": c["session_id"]}, c, t); err != nil {
				return err
			}
		}
	}
	if e = releaseEndedReservations(db, ac, t); e != nil {
		return e
	}
	if e = refreshSettlementPending(db, platform.Uint(ac["id"]), t); e != nil {
		return e
	}
	return restoreEnforcement(db, ac, t)
}

func settlePending(ctx context.Context, a *platform.App) error {
	var ids []uint64
	if e := a.DB.WithContext(ctx).Table("credit_accounts").Where("settlement_pending = TRUE AND status <> 'closed'").Order("id").Pluck("id", &ids).Error; e != nil {
		return e
	}
	for _, id := range ids {
		if e := a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
			ac, err := platform.Get(db, "credit_accounts", id, true)
			if err != nil {
				return err
			}
			return restore(db, ac, now())
		}); e != nil {
			return e
		}
	}
	return nil
}

func restoreEnforcement(db *gorm.DB, ac row, t time.Time) error {
	events, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id"), "credit_enforcements", "account_id = ? AND status IN ('grace','stopping','unknown')", ac["id"])
	if e != nil {
		return e
	}
	if len(events) == 0 {
		return nil
	}
	left, _, e := balances(db, platform.Uint(ac["id"]))
	if e != nil {
		return e
	}
	debt, e := outstandingDebt(db, platform.Uint(ac["id"]))
	if e != nil {
		return e
	}
	left = left.Sub(debt)
	if !left.IsPositive() {
		return nil
	}
	sessions, e := rows(db, "usage_sessions", "account_id = ? AND status = 'active' AND billing_mode = 'credit' AND NOT EXISTS (SELECT 1 FROM instances failed_i WHERE failed_i.id=usage_sessions.instance_id AND failed_i.creation_failed_at IS NOT NULL FOR SHARE)", ac["id"])
	if e != nil {
		return e
	}
	need := decimal.Zero
	for _, s := range sessions {
		p, e := sessionCreditPolicy(db, s)
		if e != nil {
			return e
		}
		need = need.Add(expected(s, platform.Uint(p["billing_window_seconds"])*1000000))
	}
	if left.LessThan(need) {
		return nil
	}
	for _, event := range events {
		var p row
		if e = json.Unmarshal([]byte(platform.Text(event["policy_snapshot"])), &p); e != nil {
			return e
		}
		if platform.Text(p["restore_action"]) != "cancel_pending_stop" {
			continue
		}
		if e = db.Table("credit_enforcements").Where("id = ?", event["id"]).Updates(row{"status": "cancelled", "revision": gorm.Expr("revision + 1")}).Error; e != nil {
			return e
		}
		if e = notify(db, platform.Uint(ac["user_id"]), "credit_restored", "Credit has been restored. Pending credit shutdown is cancelled; stopped instances are not restarted automatically.", "credit_enforcement", platform.Uint(event["id"])); e != nil {
			return e
		}
	}
	return nil
}
