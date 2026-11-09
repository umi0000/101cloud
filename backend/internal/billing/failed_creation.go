package billing

import (
	"context"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// These predicates read the durable instance marker, not a possibly stale worker
// snapshot. The account lock serializes marking a failure with every payment.
const chargeNotFailedSQL = `NOT EXISTS (SELECT 1 FROM usage_sessions failed_s JOIN instances failed_i ON failed_i.id=failed_s.instance_id WHERE failed_s.id=c.session_id AND failed_i.creation_failed_at IS NOT NULL FOR SHARE)`

const failedCreationRefundKeySQL = `CONCAT('failed-creation:refund:',l.id)`
const unrefundedCreationLedgerSQL = `l.entry_type='settle' AND l.amount_delta_pt<0 AND NOT EXISTS (SELECT 1 FROM credit_ledger refunded WHERE refunded.bucket_id=l.bucket_id AND refunded.business_key=` + failedCreationRefundKeySQL + ` FOR SHARE)`
const failedCreationWorkSQL = `(EXISTS (SELECT 1 FROM usage_sessions s WHERE s.instance_id=i.id AND s.status='active') OR EXISTS (SELECT 1 FROM usage_sessions s JOIN usage_charges c ON c.session_id=s.id WHERE s.instance_id=i.id AND c.status<>'cancelled') OR EXISTS (SELECT 1 FROM usage_sessions s JOIN credit_reservations r ON r.session_id=s.id WHERE s.instance_id=i.id AND r.status='held') OR EXISTS (SELECT 1 FROM usage_sessions s JOIN usage_charges c ON c.session_id=s.id JOIN credit_ledger l ON l.charge_id=c.id WHERE s.instance_id=i.id AND ` + unrefundedCreationLedgerSQL + `))`

func instanceCreationFailed(db *gorm.DB, instance row) (bool, error) {
	var marker struct{ ID uint64 }
	err := db.Clauses(clause.Locking{Strength: "SHARE"}).Table("instances").Select("id").Where("id=? AND creation_failed_at IS NOT NULL", instance["id"]).Limit(1).Find(&marker).Error
	return marker.ID != 0, err
}

func sessionCreationFailed(db *gorm.DB, session row) (bool, error) {
	var marker struct{ ID uint64 }
	err := db.Clauses(clause.Locking{Strength: "SHARE"}).Table("usage_sessions s").Select("s.id").Joins("JOIN instances i ON i.id=s.instance_id").Where("s.id=? AND i.creation_failed_at IS NOT NULL", session["id"]).Limit(1).Find(&marker).Error
	return marker.ID != 0, err
}

// FailedCreationBillingPending is independent of physical reclamation. Only a
// false result allows the task API to report billing_status=refunded. Historical
// expected/settled amounts remain intact; ledger reversals prove reimbursement.
func FailedCreationBillingPending(ctx context.Context, db *gorm.DB, instance platform.Row) (bool, error) {
	if platform.Text(instance["billing_mode"]) == "admin_exempt" {
		return false, nil
	}
	var marker struct{ ID uint64 }
	err := db.WithContext(ctx).Table("instances i").Select("i.id").Where("i.id=? AND i.creation_failed_at IS NOT NULL AND "+failedCreationWorkSQL, instance["id"]).Limit(1).Find(&marker).Error
	return marker.ID != 0, err
}

// CancelFailedCreation performs ONE bounded financial cleanup chunk in the
// caller's transaction, with its owner account already locked. A nil error means
// this chunk succeeded, not that every chunk has finished. The durable failure
// marker immediately blocks all metering and debt payments, including while the
// guest still exists. The billing poller resumes outstanding chunks after restart.
// No agent I/O, new grants, historical deletes or settled-amount rewrites occur.
func CancelFailedCreation(ctx context.Context, db *gorm.DB, instance platform.Row, failedAt time.Time) error {
	db = db.WithContext(ctx)
	current, err := platform.Get(db, "instances", platform.Uint(instance["id"]), true)
	if err != nil {
		return err
	}
	markedAt := stamp(current["creation_failed_at"])
	if markedAt.IsZero() || failedAt.IsZero() || !markedAt.Equal(failedAt) {
		return conflict("invalid_state", "failed creation billing requires its durable first-failure timestamp")
	}
	var snapshot row
	if value := decoded(current["incus_config_snapshot"]); value != nil {
		if object, ok := value.(map[string]any); ok {
			snapshot = object
		}
	}
	if snapshot["creation_delivered"] == true {
		return conflict("invalid_state", "delivered instances cannot be refunded as failed creations")
	}
	var original struct{ ID uint64 }
	if err = db.Clauses(clause.Locking{Strength: "SHARE"}).Table("tasks").Select("id").Where("id=? AND instance_id=? AND user_id=? AND task_type='instance.create'", current["creation_failure_task_id"], current["id"], current["user_id"]).Limit(1).Find(&original).Error; err != nil {
		return err
	}
	if original.ID == 0 {
		return conflict("invalid_state", "failed creation billing requires the original creation task")
	}
	var delivered struct{ ID uint64 }
	if err = db.Clauses(clause.Locking{Strength: "SHARE"}).Table("tasks t").Select("t.id").Where("t.instance_id=? AND ((t.task_type='instance.create' AND t.status='succeeded') OR EXISTS (SELECT 1 FROM task_steps s WHERE s.task_id=t.id AND s.step_key='create.verify' AND s.status='succeeded' FOR SHARE))", current["id"]).Limit(1).Find(&delivered).Error; err != nil {
		return err
	}
	if delivered.ID != 0 {
		return conflict("invalid_state", "delivered instances cannot be refunded as failed creations")
	}
	if platform.Text(current["billing_mode"]) == "admin_exempt" {
		return nil
	}
	ac, err := account(db, platform.Uint(current["user_id"]))
	if err != nil {
		return err
	}
	// Sessions close without a final paid interval. Keep historical cursors even
	// if an older charge was metered beyond the time failure was recognized.
	sessions, err := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id").Limit(settlementChunkSize), "usage_sessions", "instance_id=? AND account_id=? AND status='active'", current["id"], ac["id"])
	if err != nil {
		return err
	}
	for _, session := range sessions {
		endedAt := markedAt
		if cursor := stamp(session["last_settled_at"]); cursor.After(endedAt) {
			endedAt = cursor
		}
		if err = db.Table("usage_sessions").Where("id=?", session["id"]).Updates(row{"status": "ended", "ended_at": endedAt, "revision": gorm.Expr("revision+1")}).Error; err != nil {
			return err
		}
	}
	charges, err := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("c.id").Limit(settlementChunkSize), "usage_charges c", "c.account_id=? AND c.status<>'cancelled' AND EXISTS (SELECT 1 FROM usage_sessions s WHERE s.id=c.session_id AND s.instance_id=?)", ac["id"], current["id"])
	if err != nil {
		return err
	}
	reportChargeIDs := make([]uint64, len(charges))
	for index, charge := range charges {
		reportChargeIDs[index] = platform.Uint(charge["id"])
	}
	if err = applyBillingReportCharges(db, platform.Uint(ac["id"]), reportChargeIDs, true); err != nil {
		return err
	}
	for _, charge := range charges {
		if err = db.Table("usage_charges").Where("id=?", charge["id"]).Updates(row{"status": "cancelled", "revision": gorm.Expr("revision+1")}).Error; err != nil {
			return err
		}
	}
	reservations, err := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("r.id").Limit(settlementChunkSize), "credit_reservations r", "r.account_id=? AND r.status='held' AND EXISTS (SELECT 1 FROM usage_sessions s WHERE s.id=r.session_id AND s.instance_id=?)", ac["id"], current["id"])
	if err != nil {
		return err
	}
	t := now()
	for _, reservation := range reservations {
		if err = releaseReservation(db, reservation, t); err != nil {
			return err
		}
	}
	// A single charge can have arbitrarily many settlement sources. Bound the
	// ledger rows, not just charges, and reverse each debit exactly once.
	entries, err := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("l.id").Limit(settlementChunkSize), "credit_ledger l", "l.account_id=? AND "+unrefundedCreationLedgerSQL+" AND EXISTS (SELECT 1 FROM usage_charges c JOIN usage_sessions s ON s.id=c.session_id WHERE c.id=l.charge_id AND s.instance_id=?)", ac["id"], current["id"])
	if err != nil {
		return err
	}
	for _, entry := range entries {
		bucket, e := platform.Get(db, "credit_buckets", platform.Uint(entry["bucket_id"]), true)
		if e != nil {
			return e
		}
		value := dec(entry["amount_delta_pt"]).Neg()
		key := "failed-creation:refund:" + sid(entry["id"])
		links := row{"charge_id": entry["charge_id"], "metadata_redacted": jsonValue(row{"reason": "failed_creation", "instance_id": sid(current["id"]), "original_ledger_id": sid(entry["id"]), "failed_at": markedAt.Format(time.RFC3339Nano)})}
		if entry["reservation_id"] != nil {
			links["reservation_id"] = entry["reservation_id"]
		}
		if e = mutateBucket(db, bucket, key, "refund", value, decimal.Zero, links); e != nil {
			return e
		}
		// Refund to the original source with its original expiry, never a new
		// bucket. Expired refunds are forfeited in the SAME transaction, without
		// disturbing another session's held balance in this bucket.
		if expiry := stamp(bucket["expires_at"]); !expiry.IsZero() && !expiry.After(t) {
			if e = mutateBucket(db, bucket, "failed-creation:expire:"+sid(entry["id"]), "expire", value.Neg(), decimal.Zero, links); e != nil {
				return e
			}
		}
	}
	return refreshSettlementPending(db, platform.Uint(ac["id"]), t)
}

func cancelFailedCreations(ctx context.Context, a *platform.App) error {
	// Keyset pages keep discovery bounded, including after instances are retired
	// or physically deleted. No in-memory cursor is required for restart safety.
	var after uint64
	for {
		var instances []row
		if err := a.DB.WithContext(ctx).Table("instances i").Select("i.id,i.user_id,i.creation_failed_at").Where("i.id>? AND i.creation_failed_at IS NOT NULL AND i.billing_mode<>'admin_exempt' AND "+failedCreationWorkSQL, after).Order("i.id").Limit(settlementChunkSize).Find(&instances).Error; err != nil {
			return err
		}
		if len(instances) == 0 {
			return nil
		}
		for _, instance := range instances {
			after = platform.Uint(instance["id"])
			if err := a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
				if _, err := account(db, platform.Uint(instance["user_id"])); err != nil {
					return err
				}
				return CancelFailedCreation(ctx, db, instance, stamp(instance["creation_failed_at"]))
			}); err != nil {
				return err
			}
		}
	}
}
