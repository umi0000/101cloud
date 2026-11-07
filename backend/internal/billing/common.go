package billing

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type row = platform.Row

var zone = time.FixedZone("UTC+8", 8*60*60)
var moneyPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,13})(\.[0-9]{1,6})?$`)

func now() time.Time                      { return time.Now().In(zone).Truncate(time.Microsecond) }
func invalid(message string) error        { return platform.E(422, "validation_failed", message) }
func conflict(code, message string) error { return platform.E(409, code, message) }
func amount(s string, negative bool) (decimal.Decimal, error) {
	if !moneyPattern.MatchString(s) {
		return decimal.Zero, invalid("amount must be an exact decimal string with at most six fractional digits")
	}
	d, e := decimal.NewFromString(s)
	if e != nil || (!negative && d.IsNegative()) {
		return decimal.Zero, invalid("amount must be nonnegative")
	}
	return d, nil
}
func dec(v any) decimal.Decimal      { d, _ := decimal.NewFromString(platform.Text(v)); return d }
func money(d decimal.Decimal) string { return d.StringFixed(6) }
func sid(v any) string               { return strconv.FormatUint(platform.Uint(v), 10) }
func stamp(v any) time.Time          { return platform.Stamp(v).In(zone) }
func jsonValue(v any) string         { b, _ := json.Marshal(v); return string(b) }
func insert(db *gorm.DB, table string, v row) (row, error) {
	if e := db.Table(table).Create(&v).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	id := platform.Uint(v["id"])
	if id == 0 {
		var result struct{ ID uint64 }
		if e := db.Raw("SELECT LAST_INSERT_ID() AS id").Scan(&result).Error; e != nil {
			return nil, e
		}
		id = result.ID
	}
	return platform.Get(db, table, id, false)
}
func find(db *gorm.DB, table string, where string, args ...any) (row, error) {
	var r row
	e := db.Table(table).Where(where, args...).Take(&r).Error
	if e == gorm.ErrRecordNotFound {
		return nil, platform.E(404, "not_found", "resource not found")
	}
	return r, platform.WrapDB(e)
}
func locked(db *gorm.DB, table string, where string, args ...any) (row, error) {
	return find(db.Clauses(clause.Locking{Strength: "UPDATE"}), table, where, args...)
}
func rows(db *gorm.DB, table string, where string, args ...any) ([]row, error) {
	var r []row
	e := db.Table(table).Where(where, args...).Find(&r).Error
	return r, platform.WrapDB(e)
}
func policyFor(db *gorm.DB, source uint64) (row, error) {
	r, e := find(db, "policy_versions", "kind = 'credit' AND source_id = ?", source)
	if he, ok := e.(*platform.HTTPError); ok && he.Status == 404 {
		return nil, conflict("policy_not_configured", "an explicit published credit policy is required")
	}
	if e != nil {
		return nil, e
	}
	var cfg row
	if e = json.Unmarshal([]byte(platform.Text(r["config"])), &cfg); e != nil {
		return nil, e
	}
	if cfg == nil {
		return nil, invalid("credit policy config must be an object")
	}
	cfg["source_id"] = source
	cfg["policy_version_id"] = r["id"]
	return cfg, nil
}
func requireID(c *gin.Context) (uint64, error) { return platform.ID(c, "id") }

func validateAllowancePolicy(p row, at time.Time) error {
	effective := stamp(p["effective_at"])
	if platform.Text(p["kind"]) != "credit" || platform.Uint(p["source_id"]) == 0 || platform.Text(p["status"]) != "active" || effective.IsZero() || effective.After(at) {
		return invalid("an active, effective credit policy version is required")
	}
	return nil
}
func account(db *gorm.DB, user uint64) (row, error) {
	r, e := locked(db, "credit_accounts", "user_id = ?", user)
	if he, ok := e.(*platform.HTTPError); ok && he.Status == 404 {
		return nil, conflict("policy_not_configured", "an explicit credit allowance account is required")
	}
	return r, e
}

func usageAccount(db *gorm.DB, instance row) (row, error) {
	if platform.Text(instance["billing_mode"]) != "admin_exempt" {
		return account(db, platform.Uint(instance["user_id"]))
	}
	ac, e := locked(db, "credit_accounts", "user_id = ?", instance["user_id"])
	if he, ok := e.(*platform.HTTPError); ok && he.Status == 404 {
		return nil, nil
	}
	return ac, e
}

// ValidateAccountPolicy resolves and validates a public allowance policy without granting funds.
// Callers that will create an account hold its policy lock until their transaction commits.
func ValidateAccountPolicy(db *gorm.DB, policyID string, lock bool) (uint64, error) {
	p, source, e := platform.PolicySource(db, "credit", policyID, lock)
	if e != nil {
		return 0, e
	}
	if e = validateAllowancePolicy(p, now()); e != nil {
		return 0, e
	}
	if _, e = validatePolicy("credit", json.RawMessage(platform.Text(p["config"]))); e != nil {
		return 0, e
	}
	return source, nil
}

// CreateAccount binds an independently selected allowance profile without granting funds.
func CreateAccount(db *gorm.DB, user uint64, policyID string) (platform.Row, error) {
	source, e := ValidateAccountPolicy(db, policyID, true)
	if e != nil {
		return nil, e
	}
	return insert(db, "credit_accounts", row{"user_id": user, "credit_policy_version_id": source, "status": "active", "version": 0})
}

// CreateRegistrationAccount grants the current business cycle only for a new
// self-service account. The caller's transaction also owns user creation/audit.
// Its validity starts at registration and ends no later than the next reset.
func CreateRegistrationAccount(db *gorm.DB, user uint64, policyID string, at time.Time) error {
	ac, e := CreateAccount(db, user, policyID)
	if e != nil {
		return e
	}
	cfg, e := accountPolicy(db, ac)
	if e != nil {
		return e
	}
	cycle, e := cycleAt(cfg, at)
	if e != nil {
		return e
	}
	return grantDailyCycle(db, ac, cfg, cycle, at)
}

func instanceCreditPolicy(db *gorm.DB, instance row) (row, error) {
	// Economic source FKs are copied from the plan at admission and never follow
	// the wallet allowance binding or subsequent plan edits.
	return policyFor(db, platform.Uint(instance["credit_policy_version_id"]))
}

func sessionCreditPolicy(db *gorm.DB, session row) (row, error) {
	instance, e := platform.Get(db, "instances", platform.Uint(session["instance_id"]), false)
	if e != nil {
		return nil, e
	}
	return instanceCreditPolicy(db, instance)
}
func accountPolicy(db *gorm.DB, a row) (row, error) {
	return policyFor(db, platform.Uint(a["credit_policy_version_id"]))
}
func mutateBucket(db *gorm.DB, b row, key, kind string, delta, reserved decimal.Decimal, links row) error {
	left := dec(b["remaining_pt"]).Add(delta)
	held := dec(b["reserved_pt"]).Add(reserved)
	if left.IsNegative() || held.IsNegative() || held.GreaterThan(left) {
		return conflict("credit_exhausted", "insufficient unreserved credit")
	}
	if delta.IsZero() && reserved.IsZero() {
		return nil
	}
	changes := row{"remaining_pt": money(left), "reserved_pt": money(held), "version": gorm.Expr("version + 1"), "revision": gorm.Expr("revision + 1")}
	if e := db.Table("credit_buckets").Where("id = ?", b["id"]).Updates(changes).Error; e != nil {
		return e
	}
	l := row{"account_id": b["account_id"], "bucket_id": b["id"], "business_key": key, "entry_type": kind, "amount_delta_pt": money(delta), "reserved_delta_pt": money(reserved), "remaining_after_pt": money(left), "reserved_after_pt": money(held)}
	for k, v := range links {
		l[k] = v
	}
	if e := db.Table("credit_ledger").Create(&l).Error; e != nil {
		return e
	}
	b["remaining_pt"] = money(left)
	b["reserved_pt"] = money(held)
	return db.Table("credit_accounts").Where("id = ?", b["account_id"]).Updates(row{"version": gorm.Expr("version + 1"), "revision": gorm.Expr("revision + 1")}).Error
}
func buckets(db *gorm.DB, aid uint64, at time.Time) ([]row, error) {
	return rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("expires_at IS NULL, expires_at, id"), "credit_buckets", "account_id = ? AND valid_from <= ? AND (expires_at IS NULL OR expires_at > ?) AND remaining_pt > 0", aid, at, at)
}
func balances(db *gorm.DB, aid uint64) (decimal.Decimal, decimal.Decimal, error) {
	var s struct {
		Remaining string
		Reserved  string
	}
	e := db.Clauses(clause.Locking{Strength: "UPDATE"}).Table("credit_buckets").Select("COALESCE(SUM(CASE WHEN valid_from <= ? AND (expires_at IS NULL OR expires_at > ?) THEN remaining_pt ELSE reserved_pt END),0) remaining, COALESCE(SUM(reserved_pt),0) reserved", now(), now()).Where("account_id = ?", aid).Scan(&s).Error
	return dec(s.Remaining), dec(s.Reserved), e
}
func grant(db *gorm.DB, a row, source, key string, value decimal.Decimal, start time.Time, expiry *time.Time, actor uint64) (row, error) {
	b, e := insert(db, "credit_buckets", row{"account_id": a["id"], "source_type": source, "business_key": key, "granted_pt": money(value), "remaining_pt": money(value), "reserved_pt": "0.000000", "valid_from": start, "expires_at": expiry, "version": 0})
	if e != nil {
		return nil, e
	}
	if e = grantLedger(db, b, key, actor, nil); e != nil {
		return nil, e
	}
	return b, nil
}
func grantLedger(db *gorm.DB, b row, key string, actor uint64, links row) error {
	value := dec(b["granted_pt"])
	if !value.IsZero() {
		kind := "grant"
		if platform.Text(b["source_type"]) == "admin" {
			kind = "adjust"
		}
		l := row{"account_id": b["account_id"], "bucket_id": b["id"], "business_key": key, "entry_type": kind, "amount_delta_pt": money(value), "reserved_delta_pt": "0.000000", "remaining_after_pt": money(value), "reserved_after_pt": "0.000000"}
		if actor != 0 {
			l["actor_user_id"] = actor
		}
		for k, v := range links {
			l[k] = v
		}
		if e := db.Table("credit_ledger").Create(&l).Error; e != nil {
			return e
		}
	}
	if e := db.Table("credit_accounts").Where("id = ?", b["account_id"]).Updates(row{"version": gorm.Expr("version + 1"), "revision": gorm.Expr("revision + 1")}).Error; e != nil {
		return e
	}
	return refreshSettlementPending(db, platform.Uint(b["account_id"]), now())
}

// The caller holds the account lock. A pending marker means at least one old
// charge has a currently eligible source; expired unreserved/future funds do not
// enqueue work. Held funds remain exclusive to their original session.
func settlementPayable(db *gorm.DB, accountID uint64, t time.Time) (bool, error) {
	var charge struct{ ID uint64 }
	err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Table("usage_charges c").Select("c.id").Where("c.account_id = ? AND c.status = 'insufficient' AND "+payableChargeSQL, accountID, t, t).Limit(1).Find(&charge).Error
	return charge.ID != 0, err
}

const payableChargeSQL = chargeNotFailedSQL + ` AND (EXISTS (SELECT 1 FROM credit_buckets b WHERE b.account_id=c.account_id AND b.remaining_pt>b.reserved_pt AND b.valid_from<=? AND (b.expires_at IS NULL OR b.expires_at>?)) OR EXISTS (SELECT 1 FROM credit_reservations r WHERE r.account_id=c.account_id AND r.session_id=c.session_id AND r.status='held' AND r.reserved_pt>r.settled_pt+r.released_pt))`

func refreshSettlementPending(db *gorm.DB, accountID uint64, t time.Time) error {
	pending, e := settlementPayable(db, accountID, t)
	if e != nil {
		return e
	}
	return db.Table("credit_accounts").Where("id = ? AND settlement_pending <> ?", accountID, pending).Updates(row{"settlement_pending": pending, "revision": gorm.Expr("revision+1")}).Error
}
func parseTime(s string) (time.Time, error) {
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return t, invalid("time must include an RFC3339 timezone")
	}
	return t.In(zone).Truncate(time.Microsecond), nil
}
func public(r row) row {
	out := row{}
	for k, v := range r {
		switch k {
		case "code_hash", "business_key", "metadata_redacted", "policy_snapshot", "active_account_id", "active_instance_id", "allocation_key":
			continue
		}
		if k == "id" || k == "revision" || k == "created_by" || regexpID.MatchString(k) {
			if v == nil {
				out[k] = nil
			} else {
				out[k] = sid(v)
			}
		} else {
			switch x := v.(type) {
			case time.Time:
				out[k] = x.In(zone).Format(time.RFC3339Nano)
			case []byte:
				out[k] = string(x)
			default:
				out[k] = v
			}
		}
	}
	return out
}
func publicItem(r row) (row, error) { return public(r), nil }

// decoded returns a stored JSON column as its JSON value (object/array).
func decoded(v any) any {
	if v == nil {
		return nil
	}
	var out any
	d := json.NewDecoder(strings.NewReader(platform.Text(v)))
	d.UseNumber()
	if d.Decode(&out) != nil {
		return nil
	}
	return out
}

var regexpID = regexp.MustCompile(`(_id|_bytes|_microseconds)$`)
