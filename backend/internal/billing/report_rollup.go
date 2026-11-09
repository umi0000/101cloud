package billing

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const billingReportChunkSize = 256

type billingReportSource struct {
	ID, InstanceID                      uint64
	FromAt, ToAt                        time.Time
	ExpectedPT, CPUWeight, MemoryWeight string
	ChargeKind                          string
	Eligible                            bool
}

// WarmBillingReports completes resumable, account-serialized chunks before the
// process serves traffic. No ID high-water is used: an older uncommitted insert
// cannot be skipped by a concurrent warmer. Source flags and sums commit together.
func WarmBillingReports(ctx context.Context, db *gorm.DB) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var pending struct{ AccountID uint64 }
		if err := db.WithContext(ctx).Table("usage_charges").Select("account_id").Where("report_rollup_complete=FALSE").Order("account_id,id").Limit(1).Find(&pending).Error; err != nil {
			return fmt.Errorf("find billing report backlog: %w", err)
		}
		if pending.AccountID == 0 {
			return nil
		}
		if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if _, err := platform.Get(tx, "credit_accounts", pending.AccountID, true); err != nil {
				return err
			}
			return warmBillingReportChunk(tx, pending.AccountID)
		}); err != nil {
			return fmt.Errorf("warm billing reports for account %d: %w", pending.AccountID, err)
		}
	}
}

// Caller owns the account lock. Keeping discovery and lock acquisition separate
// avoids locking source rows before the financial account.
func warmBillingReportChunk(db *gorm.DB, accountID uint64) error {
	var charges []struct{ ID uint64 }
	if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Table("usage_charges").Select("id").Where("account_id=? AND report_rollup_complete=FALSE", accountID).Order("to_at,from_at,id").Limit(billingReportChunkSize).Find(&charges).Error; err != nil {
		return err
	}
	ids := make([]uint64, len(charges))
	for i, c := range charges {
		ids[i] = c.ID
	}
	return applyBillingReportCharges(db, accountID, ids, false)
}

// applyBillingReportCharges is also used by real settlement/cancellation inside
// their existing transaction. Cancellation uses the applied bit, not eligibility:
// the failure marker is already durable when its contribution must be reversed.
func applyBillingReportCharges(db *gorm.DB, accountID uint64, ids []uint64, reverse bool) error {
	if len(ids) == 0 {
		return nil
	}
	var sources []billingReportSource
	q := db.Table("usage_charges c").Select(`c.id,s.instance_id,c.from_at,c.to_at,c.expected_pt,s.charge_kind,
 s.cpu_hourly_pt*CAST(COALESCE(JSON_UNQUOTE(JSON_EXTRACT(s.config_snapshot,'$.vcpus')),'0') AS DECIMAL(20,0))*1073741824 AS cpu_weight,
 s.memory_gib_hourly_pt*CAST(COALESCE(JSON_UNQUOTE(JSON_EXTRACT(s.config_snapshot,'$.memory_bytes')),'0') AS DECIMAL(20,0)) AS memory_weight,
 (c.status IN ('pending','settled','insufficient') AND s.billing_mode='credit' AND i.creation_failed_at IS NULL) AS eligible`).Joins("JOIN usage_sessions s ON s.id=c.session_id AND s.account_id=c.account_id").Joins("JOIN instances i ON i.id=s.instance_id AND i.user_id=s.user_id").Where("c.account_id=? AND c.id IN ?", accountID, ids)
	if reverse {
		q = q.Where("c.report_rollup_applied=TRUE")
	} else {
		q = q.Where("c.report_rollup_complete=FALSE")
	}
	// Use a current source read even when a financial caller established an
	// earlier repeatable-read snapshot. Only c is locked; session pricing and
	// instance identity are immutable and must not add cross-account locks.
	if err := q.Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "c"}}).Scan(&sources).Error; err != nil {
		return err
	}
	type bucketKey struct {
		instance uint64
		start    time.Time
	}
	hours, days := map[bucketKey]billingTailAmounts{}, map[bucketKey]billingTailAmounts{}
	applied := make([]uint64, 0, len(sources))
	var maxTo time.Time
	for _, s := range sources {
		if !reverse && !s.Eligible {
			continue
		}
		if !s.FromAt.Before(s.ToAt) {
			return fmt.Errorf("invalid billing report charge interval %d", s.ID)
		}
		applied = append(applied, s.ID)
		if s.ToAt.After(maxTo) {
			maxTo = s.ToAt
		}
		for _, daily := range []bool{false, true} {
			buckets := hours
			if daily {
				buckets = days
			}
			for start := billingReportFloor(s.FromAt, daily); start.Before(s.ToAt); {
				end := start.Add(time.Hour)
				if daily {
					end = start.AddDate(0, 0, 1)
				}
				v := billingReportSourceClip(s, billingWindow{start, end})
				if reverse {
					v = billingTailAmounts{v.total.Neg(), v.cpu.Neg(), v.memory.Neg(), v.disk.Neg(), v.gpu.Neg()}
				}
				key := bucketKey{s.InstanceID, start}
				old := buckets[key]
				buckets[key] = billingTailAmounts{old.total.Add(v.total), old.cpu.Add(v.cpu), old.memory.Add(v.memory), old.disk.Add(v.disk), old.gpu.Add(v.gpu)}
				start = end
			}
		}
	}
	for _, daily := range []bool{false, true} {
		buckets, table := hours, "billing_report_hours"
		if daily {
			buckets, table = days, "billing_report_days"
		}
		keys := make([]bucketKey, 0, len(buckets))
		for key := range buckets {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].start.Equal(keys[j].start) {
				return keys[i].instance < keys[j].instance
			}
			return keys[i].start.Before(keys[j].start)
		})
		for offset := 0; offset < len(keys); offset += billingReportChunkSize {
			end := offset + billingReportChunkSize
			if end > len(keys) {
				end = len(keys)
			}
			values, args := make([]string, 0, end-offset), make([]any, 0, (end-offset)*8)
			for _, key := range keys[offset:end] {
				v := buckets[key]
				values = append(values, "(?,?,?,?,?,?,?,?)")
				args = append(args, accountID, key.instance, key.start, money(v.total), money(v.cpu), money(v.memory), money(v.disk), money(v.gpu))
			}
			if err := db.Exec("INSERT INTO "+table+" (account_id,instance_id,bucket_start,recorded,cpu,memory,disk,gpu) VALUES "+strings.Join(values, ",")+" ON DUPLICATE KEY UPDATE recorded=recorded+VALUES(recorded),cpu=cpu+VALUES(cpu),memory=memory+VALUES(memory),disk=disk+VALUES(disk),gpu=gpu+VALUES(gpu)", args...).Error; err != nil {
				return err
			}
		}
	}
	if !reverse && !maxTo.IsZero() {
		if err := db.Exec("INSERT INTO billing_report_state(account_id,max_to_at) VALUES (?,?) ON DUPLICATE KEY UPDATE max_to_at=GREATEST(max_to_at,VALUES(max_to_at))", accountID, maxTo).Error; err != nil {
			return err
		}
	}
	// Explicit self-assignment suppresses MySQL's ON UPDATE timestamp. Reporting
	// must not change the source revision or any original financial field.
	if reverse {
		return db.Exec("UPDATE usage_charges SET report_rollup_applied=FALSE,updated_at=updated_at WHERE account_id=? AND id IN ?", accountID, ids).Error
	}
	if err := db.Exec("UPDATE usage_charges SET report_rollup_complete=TRUE,updated_at=updated_at WHERE account_id=? AND id IN ?", accountID, ids).Error; err != nil {
		return err
	}
	if len(applied) > 0 {
		return db.Exec("UPDATE usage_charges SET report_rollup_applied=TRUE,updated_at=updated_at WHERE account_id=? AND id IN ?", accountID, applied).Error
	}
	return nil
}

func billingReportFloor(at time.Time, daily bool) time.Time {
	at = at.In(zone)
	hour := at.Hour()
	if daily {
		hour = 0
	}
	return time.Date(at.Year(), at.Month(), at.Day(), hour, 0, 0, 0, zone)
}

func billingReportSourceClip(s billingReportSource, w billingWindow) billingTailAmounts {
	from, to := w.From, w.To
	if s.FromAt.After(from) {
		from = s.FromAt
	}
	if s.ToAt.Before(to) {
		to = s.ToAt
	}
	if !from.Before(to) {
		return billingTailAmounts{}
	}
	amount := dec(s.ExpectedPT)
	duration := decimal.NewFromInt(s.ToAt.UnixMicro() - s.FromAt.UnixMicro())
	prefix := func(at time.Time) decimal.Decimal {
		return amount.Mul(decimal.NewFromInt(at.UnixMicro()-s.FromAt.UnixMicro())).DivRound(duration, 6)
	}
	lo, hi := prefix(from), prefix(to)
	v := billingTailAmounts{total: hi.Sub(lo)}
	switch s.ChargeKind {
	case "base_compute":
		cpu, weight := dec(s.CPUWeight), dec(s.CPUWeight).Add(dec(s.MemoryWeight))
		if weight.IsPositive() {
			v.cpu = hi.Mul(cpu).DivRound(weight, 6).Sub(lo.Mul(cpu).DivRound(weight, 6))
		}
		v.memory = v.total.Sub(v.cpu)
	case "storage":
		v.disk = v.total
	case "gpu":
		v.gpu = v.total
	}
	return v
}
