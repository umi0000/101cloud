package billing

import (
	"context"
	"encoding/json"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// eventAvailable uses one clock for both sides of a grant. Expired/future held
// funds are not available, and unpaid insufficient charges reduce availability.
// The caller already owns the account lock. Current reads deliberately bypass a
// repeatable-read snapshot established before waiting for that account lock.
func eventAvailable(db *gorm.DB, accountID uint64, at time.Time) (string, error) {
	var debt, free string
	if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Table("usage_charges c").Select("COALESCE(SUM(c.expected_pt-c.settled_pt),0)").Where("c.account_id=? AND c.status='insufficient' AND "+chargeNotFailedSQL, accountID).Scan(&debt).Error; err != nil {
		return "", err
	}
	if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Table("credit_buckets").Select("COALESCE(SUM(remaining_pt-reserved_pt),0)").Where("account_id=? AND valid_from<=? AND (expires_at IS NULL OR expires_at>?)", accountID, at, at).Scan(&free).Error; err != nil {
		return "", err
	}
	return money(dec(free).Sub(dec(debt))), nil
}

func recordGrantEvent(db *gorm.DB, bucket row, before string, at time.Time) error {
	after, err := eventAvailable(db, platform.Uint(bucket["account_id"]), at)
	if err != nil {
		return err
	}
	var owner struct{ UserID uint64 }
	if err = db.Table("credit_accounts").Clauses(clause.Locking{Strength: "UPDATE"}).Select("user_id").Where("id=?", bucket["account_id"]).Take(&owner).Error; err != nil {
		return err
	}
	_, err = insert(db, "billing_events", row{"user_id": owner.UserID, "account_id": bucket["account_id"], "business_key": "grant:" + sid(bucket["id"]), "event_type": "credit.grant", "occurred_at": at, "grant_source": bucket["source_type"], "amount_pt": money(dec(bucket["granted_pt"])), "available_before_pt": before, "available_after_pt": after, "published": true})
	return err
}

// eventHourly prices only frozen, active metering sessions, never admission
// reservations or today's catalog prices. Account serialization makes concurrent
// instance changes visible without taking any other instance's lock.
func eventHourly(db *gorm.DB, user uint64, at time.Time, confirmedStopped ...uint64) (*string, error) {
	var ac row
	err := db.Table("credit_accounts").Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("user_id=?", user).Take(&ac).Error
	if err == gorm.ErrRecordNotFound {
		zero := money(decimal.Zero)
		return &zero, nil
	}
	if err != nil {
		return nil, err
	}
	// OF s is essential: a join must never lock other instances after this
	// transaction has already locked its own instance.
	ss, err := rows(db.Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "s"}}).Select("s.instance_id,s.billing_mode,s.metering_state,s.charge_kind,s.config_snapshot,s.cpu_hourly_pt,s.memory_gib_hourly_pt,s.disk_gib_hourly_pt,s.gpu_hourly_pt,p.config event_policy,n.health event_node_health").Joins("JOIN instances i ON i.id=s.instance_id").Joins("LEFT JOIN policy_versions p ON p.kind='credit' AND p.source_id=i.credit_policy_version_id").Joins("LEFT JOIN nodes n ON n.id=s.node_id").Order("s.id"), "usage_sessions s", "s.account_id=? AND s.status='active' AND i.creation_failed_at IS NULL AND i.deleted_at IS NULL AND i.billing_mode<>'admin_exempt'", ac["id"])
	if err != nil {
		return nil, err
	}
	var instances []row
	if err = db.Table("credit_accounts a").Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "a"}}).Select("i.id,i.observed_state").Joins("JOIN instances i ON i.user_id=a.user_id").Where("a.id=? AND i.creation_failed_at IS NULL AND i.deleted_at IS NULL AND i.billing_mode<>'admin_exempt'", ac["id"]).Find(&instances).Error; err != nil {
		return nil, err
	}
	compute := make(map[uint64]bool)
	for _, s := range ss {
		if platform.Text(s["charge_kind"]) == "base_compute" && in(platform.Text(s["metering_state"]), "held", "running") {
			compute[platform.Uint(s["instance_id"])] = true
		}
	}
	for _, i := range instances {
		if len(confirmedStopped) > 0 && platform.Uint(i["id"]) == confirmedStopped[0] {
			continue
		}
		if platform.Text(i["observed_state"]) != "stopped" && !compute[platform.Uint(i["id"])] {
			return nil, nil
		}
	}
	var enforcements []row
	if err = db.Table("credit_enforcements e").Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "e"}}).Select("m.instance_id,e.exhausted_at,e.policy_snapshot").Joins("JOIN credit_enforcement_instances m ON m.enforcement_id=e.id").Where("e.account_id=? AND e.status IN ('grace','stopping','unknown')", ac["id"]).Find(&enforcements).Error; err != nil {
		return nil, err
	}
	waived := make(map[uint64]bool)
	for _, e := range enforcements {
		instanceID := platform.Uint(e["instance_id"])
		if _, exists := waived[instanceID]; exists {
			return nil, nil
		}
		waived[instanceID] = false
		var p row
		if json.Unmarshal([]byte(platform.Text(e["policy_snapshot"])), &p) != nil || !in(platform.Text(p["grace_charge_mode"]), "waived", "accrue") || stamp(e["exhausted_at"]).IsZero() {
			return nil, nil
		}
		if !at.Before(stamp(e["exhausted_at"])) && platform.Text(p["grace_charge_mode"]) == "waived" {
			waived[platform.Uint(e["instance_id"])] = true
		}
	}
	total := decimal.Zero
	for _, s := range ss {
		if platform.Text(s["billing_mode"]) == "admin_exempt" || platform.Text(s["metering_state"]) == "held" || waived[platform.Uint(s["instance_id"])] {
			continue
		}
		if platform.Text(s["metering_state"]) != "running" {
			return nil, nil
		}
		var policy row
		if json.Unmarshal([]byte(platform.Text(s["event_policy"])), &policy) != nil || !in(platform.Text(policy["storage_charge_mode"]), "running_only", "free") || !in(platform.Text(policy["offline_charge_mode"]), "defer", "continue") {
			return nil, nil
		}
		if platform.Text(policy["offline_charge_mode"]) == "defer" && platform.Text(s["event_node_health"]) != "online" {
			return nil, nil
		}
		rate, _, valid := billingSessionHourly(s)
		if !valid {
			return nil, nil
		}
		total = total.Add(rate)
	}
	value := money(total)
	return &value, nil
}

// PrepareInstanceEvent is called before ending/opening sessions, inside the
// lifecycle transaction and after its account lock. Finish runs after metering
// changes in that same transaction. A draft creation is published only after
// successful delivery; retries/reconciliation reuse the original business key.
func PrepareInstanceEvent(db *gorm.DB, instance row, eventType, key string, at time.Time, published bool) (func() error, error) {
	user := platform.Uint(instance["user_id"])
	before, err := eventHourly(db, user, at)
	if err != nil {
		return nil, err
	}
	return func() error {
		var stopped uint64
		if eventType == "instance.stop" || eventType == "instance.delete" {
			stopped = platform.Uint(instance["id"])
		}
		after, err := eventHourly(db, user, at, stopped)
		if err != nil {
			return err
		}
		var ac row
		err = db.Table("credit_accounts").Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("user_id=?", user).Take(&ac).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return err
		}
		var aid any
		if ac != nil {
			aid = ac["id"]
		}
		event := row{"user_id": user, "account_id": aid, "business_key": key, "event_type": eventType, "occurred_at": at, "instance_id": instance["id"], "instance_name": instance["logical_name"], "hourly_before_pt": before, "hourly_after_pt": after, "published": published}
		if err = db.Table("billing_events").Clauses(clause.OnConflict{DoUpdates: clause.Assignments(row{"id": gorm.Expr("id")})}).Create(&event).Error; err != nil {
			return err
		}
		if published {
			return PublishInstanceEvent(db, user, key)
		}
		return nil
	}, nil
}

func PublishInstanceEvent(db *gorm.DB, user uint64, key string) error {
	return db.Table("billing_events").Where("user_id=? AND business_key=? AND published=FALSE", user, key).Update("published", true).Error
}

// ConfirmInstanceUsage records a confirmed metering transition in its caller's
// transaction; the caller holds the account lock before its instance lock.
func ConfirmInstanceUsage(ctx context.Context, db *gorm.DB, instance row, usageEvent, eventType, key string, at time.Time) error {
	finish, err := PrepareInstanceEvent(db, instance, eventType, key, at, true)
	if err != nil {
		return err
	}
	if err = instanceUsageAt(ctx, db, instance, usageEvent, nil, at); err != nil {
		return err
	}
	return finish()
}
