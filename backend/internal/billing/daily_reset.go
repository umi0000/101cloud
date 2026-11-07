package billing

import (
	"context"
	"fmt"
	"time"
	_ "time/tzdata" // Business calendars must not depend on host/MySQL timezone tables.

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Construct each boundary from its calendar date and policy clock, rather than
// adding 24 hours or carrying a DST-normalized clock into subsequent dates.
func nextDailyReset(p row, cycle time.Time) (time.Time, error) {
	if platform.Text(p["timezone"]) == "" {
		return time.Time{}, invalid("daily policy timezone is required")
	}
	loc, err := time.LoadLocation(platform.Text(p["timezone"]))
	if err != nil {
		return time.Time{}, err
	}
	clock, err := time.Parse("15:04:05", platform.Text(p["grant_time"]))
	if err != nil {
		return time.Time{}, err
	}
	local := cycle.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day()+1, clock.Hour(), clock.Minute(), clock.Second(), 0, loc), nil
}

func dailyExpiry(p row, cycle, validFrom time.Time) (time.Time, error) {
	end, err := nextDailyReset(p, cycle)
	if err != nil {
		return time.Time{}, err
	}
	seconds := platform.Uint(p["daily_validity_seconds"])
	// Validated policies use positive durations. Reject corrupt persisted values
	// explicitly rather than overflow a duration or create a non-expiring grant.
	if seconds == 0 || seconds > uint64((1<<63-1)/int64(time.Second)) {
		return time.Time{}, invalid("invalid daily validity in stored policy")
	}
	if ttl := validFrom.Add(time.Duration(seconds) * time.Second); ttl.Before(end) {
		end = ttl
	}
	if !validFrom.Before(end) {
		return time.Time{}, invalid("daily grant does not precede its next reset")
	}
	return end.In(zone), nil
}

// NormalizeDailyBuckets is the pre-serve/pre-worker financial cutover. Run with
// old application processes stopped; account locks serialize updated processes.
// Keyset discovery and one bucket per transaction bound memory and lock duration.
// All daily sources, including closed accounts and fully consumed buckets, are
// covered: future failed-create refunds must retain the corrected original expiry.
// A crash can leave committed prefixes; replay is safe and never extends expiry.
func NormalizeDailyBuckets(ctx context.Context, root *gorm.DB) error {
	db := root.WithContext(ctx)
	var invalidGrant struct{ ID, AccountID uint64 }
	if err := db.Table("credit_daily_grants g").Select("g.id, g.account_id").Joins("LEFT JOIN credit_buckets b ON b.id=g.bucket_id").Where("b.id IS NULL OR b.source_type<>'daily' OR b.account_id<>g.account_id OR EXISTS (SELECT 1 FROM credit_daily_grants g2 WHERE g2.bucket_id=g.bucket_id AND g2.id<>g.id)").Order("g.id").Limit(1).Scan(&invalidGrant).Error; err != nil {
		return err
	}
	if invalidGrant.ID != 0 {
		return fmt.Errorf("daily reset cutover account %d grant %d: missing, duplicate, or mismatched bucket history", invalidGrant.AccountID, invalidGrant.ID)
	}
	var after uint64
	for {
		var page []struct{ ID, AccountID uint64 }
		if err := db.Table("credit_buckets b").Select("b.id, b.account_id").Where("b.source_type='daily' AND b.id>? AND NOT EXISTS (SELECT 1 FROM credit_daily_grants g WHERE g.bucket_id=b.id AND g.reset_capped=TRUE)", after).Order("b.id").Limit(settlementChunkSize).Scan(&page).Error; err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, item := range page {
			if err := db.Transaction(func(tx *gorm.DB) error {
				ac, err := platform.Get(tx, "credit_accounts", item.AccountID, true)
				if err != nil {
					return err
				}
				b, err := platform.Get(tx, "credit_buckets", item.ID, true)
				if err != nil {
					return err
				}
				return normalizeDailyBucket(tx, ac, b, now())
			}); err != nil {
				return fmt.Errorf("daily reset cutover account %d bucket %d: %w", item.AccountID, item.ID, err)
			}
			after = item.ID
		}
	}
}

func normalizeDailyBucket(db *gorm.DB, ac, b row, at time.Time) error {
	grants, err := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Limit(2), "credit_daily_grants", "bucket_id=?", b["id"])
	if err != nil {
		return err
	}
	if len(grants) != 1 || platform.Uint(grants[0]["account_id"]) != platform.Uint(ac["id"]) {
		return fmt.Errorf("daily bucket requires exactly one matching daily grant")
	}
	g := grants[0]
	if platform.Uint(g["reset_capped"]) == 1 {
		return nil
	}
	// The historical grant's source FK is authoritative, never the account's
	// possibly switched allowance profile. Source rows retain the policy clock.
	p, err := platform.Get(db, "credit_policy_versions", platform.Uint(g["policy_version_id"]), false)
	if err != nil {
		return err
	}
	p["grant_time"] = p["reset_time"]
	date := stamp(g["business_date"])
	if date.IsZero() || stamp(g["valid_from"]).IsZero() || stamp(g["expires_at"]).IsZero() || stamp(b["expires_at"]).IsZero() || !stamp(g["valid_from"]).Equal(stamp(b["valid_from"])) || platform.Text(b["business_key"]) != "daily:"+date.Format("2006-01-02") {
		return fmt.Errorf("invalid daily grant date, key, or validity history")
	}
	loc, err := time.LoadLocation(platform.Text(p["timezone"]))
	if err != nil {
		return err
	}
	// business_date is a SQL DATE in the connection zone, not an instant in the
	// policy zone. Preserve its calendar components when deriving the boundary.
	day := time.Date(date.Year(), date.Month(), date.Day(), 12, 0, 0, 0, loc)
	end, err := nextDailyReset(p, day)
	if err != nil {
		return err
	}
	end = end.In(zone)
	for _, previous := range []time.Time{stamp(b["expires_at"]), stamp(g["expires_at"])} {
		if previous.Before(end) {
			end = previous
		}
	}
	if !stamp(b["valid_from"]).Before(end) {
		return fmt.Errorf("historical policy reset does not follow daily valid_from")
	}
	changed := !stamp(b["expires_at"]).Equal(end) || !stamp(g["expires_at"]).Equal(end)
	if changed {
		if err = db.Table("credit_buckets").Where("id=?", b["id"]).Updates(row{"expires_at": end, "version": gorm.Expr("version+1"), "revision": gorm.Expr("revision+1")}).Error; err != nil {
			return err
		}
		if err = db.Table("credit_accounts").Where("id=?", ac["id"]).Updates(row{"version": gorm.Expr("version+1"), "revision": gorm.Expr("revision+1")}).Error; err != nil {
			return err
		}
	}
	if err = db.Table("credit_daily_grants").Where("id=?", g["id"]).Updates(row{"expires_at": end, "reset_capped": true, "revision": gorm.Expr("revision+1")}).Error; err != nil {
		return err
	}
	if free := dec(b["remaining_pt"]).Sub(dec(b["reserved_pt"])); !end.After(at) && free.IsPositive() {
		key, err := randomKey()
		if err != nil {
			return err
		}
		if err = mutateBucket(db, b, "expire:"+key, "expire", free.Neg(), decimal.Zero, nil); err != nil {
			return err
		}
	}
	return refreshSettlementPending(db, platform.Uint(ac["id"]), at)
}
