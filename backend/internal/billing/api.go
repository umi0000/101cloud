package billing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func randomKey() (string, error) {
	var b [24]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return hex.EncodeToString(b[:]), nil
}
func Register(a *platform.App, user, admin *gin.RouterGroup) {
	registerPolicies(a, admin)
	user.GET("/credits/account", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := find(platform.DB(c, a), "credit_accounts", "user_id = ?", platform.Current(c).ID)
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return accountDTO(platform.DB(c, a), r)
	}))
	user.GET("/credits/buckets", platform.Handle(func(c *gin.Context) (any, error) {
		db := platform.DB(c, a)
		q := db.Table("credit_buckets").Select("credit_buckets.id,credit_buckets.account_id,source_type,granted_pt,remaining_pt,reserved_pt,valid_from,expires_at,credit_buckets.revision,credit_buckets.created_at,credit_buckets.updated_at").Where("account_id IN (SELECT id FROM credit_accounts WHERE user_id = ?)", platform.Current(c).ID)
		if s := c.Query("source_type"); s != "" {
			if !in(s, "daily", "redemption", "admin") {
				return nil, invalid("unsupported bucket source")
			}
			q = q.Where("source_type = ?", s)
		}
		if s := c.Query("active_only"); s != "" {
			if !in(s, "true", "false") {
				return nil, invalid("active_only must be boolean")
			}
			if s == "true" {
				q = q.Where("valid_from <= ? AND (expires_at IS NULL OR expires_at > ?)", now(), now())
			}
		}
		return platform.List(c, a, q, []string{"source_type", "active_only"})
	}))
	registerFinancialList(a, user, "/credits/ledger", "credit_ledger", false)
	registerFinancialList(a, user, "/usage/sessions", "usage_sessions", false)
	registerFinancialList(a, user, "/usage/charges", "usage_charges", false)
	user.GET("/credits/redemptions", platform.Handle(func(c *gin.Context) (any, error) {
		return platform.ListMap(c, a, platform.DB(c, a).Table("redemption_records").Where("user_id = ?", platform.Current(c).ID), nil, publicItem)
	}))
	user.POST("/credits/redemptions", platform.Handle(func(c *gin.Context) (any, error) {
		var b struct {
			Code string `json:"code"`
		}
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		if len(b.Code) < 24 || len(b.Code) > 256 {
			return nil, invalid("invalid redemption code")
		}
		db := platform.DB(c, a)
		ac, e := account(db, platform.Current(c).ID, true)
		if e != nil {
			return nil, e
		}
		if platform.Text(ac["status"]) != "active" {
			return nil, conflict("invalid_state", "credit account is not active")
		}
		hash := sha256.Sum256([]byte(b.Code))
		code, e := locked(db, "redemption_codes", "code_hash = ?", hash[:])
		if e != nil {
			return nil, invalid("invalid redemption code")
		}
		var existing row
		er := db.Table("redemption_records").Where("code_id = ? AND user_id = ?", code["id"], platform.Current(c).ID).Take(&existing).Error
		if er == nil {
			platform.Reply(c, 201, public(existing))
			return nil, nil
		}
		if er != gorm.ErrRecordNotFound {
			return nil, er
		}
		t := now()
		if platform.Text(code["status"]) != "active" || t.Before(stamp(code["valid_from"])) || !t.Before(stamp(code["expires_at"])) || platform.Uint(code["used_count"]) >= platform.Uint(code["max_uses"]) {
			return nil, conflict("invalid_state", "redemption code is disabled, expired or fully redeemed")
		}
		var expiry *time.Time
		if sec := platform.Uint(code["bucket_validity_seconds"]); sec > 0 {
			x := t.Add(time.Duration(sec) * time.Second)
			expiry = &x
		}
		key := "redemption:" + sid(code["id"]) + ":" + sid(ac["user_id"])
		bucket, e := insert(db, "credit_buckets", row{"account_id": ac["id"], "source_type": "redemption", "business_key": key, "granted_pt": code["grant_pt"], "remaining_pt": code["grant_pt"], "reserved_pt": "0.000000", "valid_from": t, "expires_at": expiry})
		if e != nil {
			return nil, e
		}
		record, e := insert(db, "redemption_records", row{"code_id": code["id"], "user_id": ac["user_id"], "account_id": ac["id"], "bucket_id": bucket["id"], "granted_pt": code["grant_pt"], "redeemed_at": t, "expires_at": expiry})
		if e != nil {
			return nil, e
		}
		if e = grantLedger(db, bucket, key, platform.Current(c).ID, row{"redemption_record_id": record["id"]}); e != nil {
			return nil, e
		}
		if e = db.Table("redemption_codes").Where("id = ?", code["id"]).Updates(row{"used_count": gorm.Expr("used_count + 1"), "revision": gorm.Expr("revision + 1")}).Error; e != nil {
			return nil, e
		}
		if e = refreshSettlementPending(db, platform.Uint(ac["id"]), t); e != nil {
			return nil, e
		}
		platform.Reply(c, 201, public(record))
		return nil, nil
	}))
	user.GET("/notifications", platform.Handle(func(c *gin.Context) (any, error) {
		q := platform.DB(c, a).Table("notifications").Where("user_id = ?", platform.Current(c).ID)
		if s := c.Query("unread_only"); s != "" {
			if !in(s, "true", "false") {
				return nil, invalid("unread_only must be boolean")
			}
			if s == "true" {
				q = q.Where("read_at IS NULL")
			}
		}
		if s := c.Query("type"); s != "" {
			q = q.Where("type = ?", s)
		}
		return platform.ListMap(c, a, q, []string{"unread_only", "type"}, publicItem)
	}))
	user.PUT("/notifications/:id/read", platform.Handle(func(c *gin.Context) (any, error) {
		var b struct {
			Read bool `json:"read"`
		}
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		if !b.Read {
			return nil, invalid("read must be true")
		}
		id, e := requireID(c)
		if e != nil {
			return nil, e
		}
		r, e := platform.Owned(c, a, "notifications", id)
		if e != nil {
			return nil, e
		}
		if e = platform.Revision(c, r); e != nil {
			return nil, e
		}
		if r["read_at"] == nil {
			r, e = platform.Save(c, a, "notifications", r, row{"read_at": now()})
			if e != nil {
				return nil, e
			}
		}
		platform.SetETag(c, r)
		return public(r), nil
	}))
	admin.GET("/credit-accounts", platform.Handle(func(c *gin.Context) (any, error) {
		db := platform.DB(c, a)
		q := db.Table("credit_accounts")
		if s := c.Query("user_id"); s != "" {
			id, e := platform.IDString(s)
			if e != nil {
				return nil, e
			}
			q = q.Where("user_id = ?", id)
		}
		return platform.ListMap(c, a, q, []string{"user_id"}, func(r row) (row, error) { return accountDTO(db, r) })
	}))
	admin.GET("/credit-accounts/:id", platform.Handle(func(c *gin.Context) (any, error) {
		id, e := requireID(c)
		if e != nil {
			return nil, e
		}
		r, e := platform.Get(platform.DB(c, a), "credit_accounts", id, false)
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return accountDTO(platform.DB(c, a), r)
	}))
	admin.PATCH("/credit-accounts/:id", platform.Handle(func(c *gin.Context) (any, error) {
		var input struct {
			PolicyID string `json:"policy_version_id"`
		}
		if e := platform.Decode(c, &input); e != nil {
			return nil, e
		}
		id, e := requireID(c)
		if e != nil {
			return nil, e
		}
		policyID, e := platform.IDString(input.PolicyID)
		if e != nil {
			return nil, e
		}
		db := platform.DB(c, a)
		ac, e := platform.Get(db, "credit_accounts", id, true)
		if e != nil {
			return nil, e
		}
		if e = platform.Revision(c, ac); e != nil {
			return nil, e
		}
		var active int64
		if e = db.Table("usage_sessions").Where("account_id=? AND status='active'", id).Count(&active).Error; e != nil {
			return nil, e
		}
		if active > 0 {
			return nil, conflict("configuration_conflict", "close active resource usage intervals before changing account policy")
		}
		if e = db.Table("credit_enforcements").Where("account_id=? AND status IN ('grace','stopping','unknown')", id).Count(&active).Error; e != nil {
			return nil, e
		}
		if active > 0 {
			return nil, conflict("configuration_conflict", "resolve the active shutdown event before changing account policy")
		}
		policy, e := platform.Get(db, "policy_versions", policyID, true)
		if e != nil {
			return nil, e
		}
		if e = validateBindingVersion("credit", policy, now()); e != nil {
			return nil, e
		}
		var next CreditConfig
		if _, e = validatePolicy("credit", json.RawMessage(platform.Text(policy["config"]))); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(platform.Text(policy["config"])), &next); e != nil {
			return nil, e
		}
		previous, e := accountPolicy(db, ac)
		if e != nil {
			return nil, e
		}
		if ac["grant_cursor_at"] != nil && (platform.Text(previous["timezone"]) != next.Timezone || platform.Text(previous["grant_time"]) != next.GrantTime) {
			return nil, conflict("configuration_conflict", "an existing grant calendar requires an explicit calendar migration")
		}
		ac, e = platform.Save(c, a, "credit_accounts", ac, row{"credit_policy_version_id": policy["source_id"], "version": gorm.Expr("version+1")})
		if e != nil {
			return nil, e
		}
		if e = platform.Audit(c.Request.Context(), db, platform.Current(c).ID, 0, 0, "credit.policy_changed", row{"account_id": sid(ac["id"]), "policy_version_id": input.PolicyID}); e != nil {
			return nil, e
		}
		return accountDTO(db, ac)
	}))
	admin.POST("/credit-accounts/:id/adjustments", platform.Handle(func(c *gin.Context) (any, error) {
		var b struct {
			Amount  string  `json:"amount_pt"`
			Expires *string `json:"expires_at"`
		}
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		value, e := amount(b.Amount, true)
		if e != nil {
			return nil, e
		}
		if value.IsZero() {
			return nil, invalid("nonzero amount is required")
		}
		id, e := requireID(c)
		if e != nil {
			return nil, e
		}
		db := platform.DB(c, a)
		ac, e := platform.Get(db, "credit_accounts", id, true)
		if e != nil {
			return nil, e
		}
		if e = platform.Revision(c, ac); e != nil {
			return nil, e
		}
		key, e := randomKey()
		if e != nil {
			return nil, e
		}
		key = "adjust:" + key
		var expiry *time.Time
		if b.Expires != nil {
			t, e := parseTime(*b.Expires)
			if e != nil {
				return nil, e
			}
			if !t.After(now()) {
				return nil, invalid("expires_at must be in the future")
			}
			expiry = &t
		}
		var bucketID any
		if value.IsPositive() {
			var bucket row
			bucket, e = grant(db, ac, "admin", key, value, now(), expiry, platform.Current(c).ID)
			if e == nil {
				bucketID = sid(bucket["id"])
			}
		} else {
			bs, x := buckets(db, id, now())
			if x != nil {
				return nil, x
			}
			need := value.Neg()
			left, held, err := balances(db, id)
			if err != nil {
				return nil, err
			}
			debt, err := outstandingDebt(db, id)
			if err != nil {
				return nil, err
			}
			if left.Sub(held).Sub(debt).LessThan(need) {
				return nil, conflict("credit_exhausted", "adjustment would consume credit owed to historical debt or reservations")
			}
			for _, bucket := range bs {
				v := decimal.Min(need, dec(bucket["remaining_pt"]).Sub(dec(bucket["reserved_pt"])))
				if v.IsPositive() {
					if e = mutateBucket(db, bucket, key, "adjust", v.Neg(), decimal.Zero, row{"actor_user_id": platform.Current(c).ID}); e != nil {
						return nil, e
					}
					need = need.Sub(v)
				}
			}
			if need.IsPositive() {
				return nil, conflict("credit_exhausted", "adjustment would consume reserved credit or make a bucket negative")
			}
		}
		if e != nil {
			return nil, e
		}
		if e = platform.Audit(c.Request.Context(), db, platform.Current(c).ID, 0, 0, "credit.adjustment", row{"account_id": sid(ac["id"]), "amount_pt": money(value)}); e != nil {
			return nil, e
		}
		debtAfter, e := outstandingDebt(db, id)
		if e != nil {
			return nil, e
		}
		left, held, e := balances(db, id)
		if e != nil {
			return nil, e
		}
		fresh, e := platform.Get(db, "credit_accounts", id, false)
		if e != nil {
			return nil, e
		}
		var expiresAt any
		if value.IsPositive() && expiry != nil {
			expiresAt = expiry.In(zone).Format(time.RFC3339Nano)
		}
		platform.SetETag(c, fresh)
		platform.Reply(c, 201, row{"account_id": sid(id), "amount_pt": money(value), "bucket_id": bucketID, "expires_at": expiresAt, "settled_debt_pt": "0.000000", "settlement_pending": platform.Bool(fresh["settlement_pending"]), "outstanding_debt_pt": money(debtAfter), "remaining_pt": money(left.Sub(debtAfter)), "reserved_pt": money(held), "available_pt": money(left.Sub(held).Sub(debtAfter)), "revision": sid(fresh["revision"])})
		return nil, nil
	}))
	registerFinancialList(a, admin, "/credit-ledger", "credit_ledger", true)
	admin.POST("/redemption-codes", platform.Handle(func(c *gin.Context) (any, error) {
		var b struct {
			Amount  string `json:"amount_pt"`
			Expires string `json:"expires_at"`
			Max     uint32 `json:"max_redemptions"`
			Count   int    `json:"count"`
		}
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		v, e := amount(b.Amount, false)
		if e != nil {
			return nil, e
		}
		end, e := parseTime(b.Expires)
		if e != nil {
			return nil, e
		}
		if !v.IsPositive() || b.Max == 0 || b.Count < 1 || b.Count > 1000 || !end.After(now()) {
			return nil, invalid("invalid redemption amount, expiry, count or redemption limit")
		}
		records := []row{}
		ids := []string{}
		for range b.Count {
			secret, e := randomKey()
			if e != nil {
				return nil, e
			}
			digest := sha256.Sum256([]byte(secret))
			r, e := insert(platform.DB(c, a), "redemption_codes", row{"code_hash": digest[:], "display_prefix": secret[:8], "grant_pt": money(v), "max_uses": b.Max, "used_count": 0, "valid_from": now(), "expires_at": end, "status": "active", "created_by": platform.Current(c).ID})
			if e != nil {
				return nil, e
			}
			out := public(r)
			out["code"] = secret
			records = append(records, out)
			ids = append(ids, sid(r["id"]))
		}
		c.Header("Cache-Control", "no-store")
		c.Set("idempotency_replay", row{"record_ids": ids, "codes_revealable": false})
		platform.Reply(c, 201, row{"items": records, "codes_revealable": true})
		return nil, nil
	}))
	admin.GET("/redemption-codes", platform.Handle(func(c *gin.Context) (any, error) {
		q := platform.DB(c, a).Table("redemption_codes")
		if s := c.Query("status"); s != "" {
			if !in(s, "active", "disabled") {
				return nil, invalid("invalid code status")
			}
			q = q.Where("status = ?", s)
		}
		return platform.ListMap(c, a, q, []string{"status"}, publicItem)
	}))
	admin.POST("/redemption-codes/:id/disable", platform.Handle(func(c *gin.Context) (any, error) {
		if e := platform.Decode(c, &struct{}{}); e != nil {
			return nil, e
		}
		id, e := requireID(c)
		if e != nil {
			return nil, e
		}
		r, e := platform.Get(platform.DB(c, a), "redemption_codes", id, true)
		if e != nil {
			return nil, e
		}
		r, e = platform.Save(c, a, "redemption_codes", r, row{"status": "disabled"})
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return public(r), nil
	}))
	registerEnforcements(a, admin)
	a.AddPoller("credit-daily-grants", time.Minute, func(ctx context.Context, a *platform.App) error { return dailyGrants(ctx, a) })
	a.AddPoller("credit-settlement", 5*time.Second, func(ctx context.Context, a *platform.App) error { return settleAll(ctx, a) })
	a.AddPoller("credit-debt-settlement", time.Second, func(ctx context.Context, a *platform.App) error { return settlePending(ctx, a) })
	a.AddPoller("credit-enforcement", time.Second, func(ctx context.Context, a *platform.App) error { return enforceAll(ctx, a) })
	a.RegisterJob("credit_enforcement_reconcile", func(ctx context.Context, a *platform.App, j platform.Job) error {
		var p struct {
			ID uint64 `json:"enforcement_id"`
		}
		if e := json.Unmarshal(j.Parameters, &p); e != nil {
			return e
		}
		return reconcileEvent(ctx, a, p.ID)
	})
}
func registerFinancialList(a *platform.App, g *gin.RouterGroup, path, table string, admin bool) {
	g.GET(path, platform.Handle(func(c *gin.Context) (any, error) {
		db := platform.DB(c, a)
		fields := map[string]string{"credit_ledger": "id,account_id,bucket_id,entry_type,amount_delta_pt,reserved_delta_pt,remaining_after_pt,reserved_after_pt,charge_id,reservation_id,redemption_record_id,revision,created_at", "usage_sessions": "id,account_id,user_id,instance_id,node_id,charge_kind,gpu_allocation_id,pricing_version_id,gpu_price_version_id,billing_mode,config_snapshot,cpu_hourly_pt,memory_gib_hourly_pt,disk_gib_hourly_pt,gpu_hourly_pt,status,started_at,last_settled_at,ended_at,revision,created_at,updated_at", "usage_charges": "id,session_id,account_id,from_at,to_at,expected_pt,settled_pt,status,calculation_snapshot,settled_at,revision,created_at,updated_at"}[table]
		q := db.Table(table).Select(fields)
		if !admin {
			q = q.Where("account_id IN (SELECT id FROM credit_accounts WHERE user_id = ?)", platform.Current(c).ID)
		} else if s := c.Query("user_id"); s != "" {
			id, e := platform.IDString(s)
			if e != nil {
				return nil, e
			}
			q = q.Where("account_id IN (SELECT id FROM credit_accounts WHERE user_id = ?)", id)
		}
		if s := c.Query("instance_id"); s != "" {
			id, e := platform.IDString(s)
			if e != nil {
				return nil, e
			}
			if table == "usage_sessions" {
				q = q.Where("instance_id = ?", id)
			} else if table == "usage_charges" {
				q = q.Where("session_id IN (SELECT id FROM usage_sessions WHERE instance_id = ?)", id)
			} else {
				q = q.Where("(charge_id IN (SELECT c.id FROM usage_charges c JOIN usage_sessions s ON s.id=c.session_id WHERE s.instance_id=?) OR reservation_id IN (SELECT r.id FROM credit_reservations r JOIN usage_sessions s ON s.id=r.session_id WHERE s.instance_id=?))", id, id)
			}
		}
		column := "created_at"
		if table == "usage_sessions" {
			column = "started_at"
		}
		if table == "usage_charges" {
			column = "from_at"
		}
		var from, to *time.Time
		for _, name := range []string{"from", "to"} {
			if s := c.Query(name); s != "" {
				t, e := parseTime(s)
				if e != nil {
					return nil, e
				}
				if name == "from" {
					from = &t
					q = q.Where(column+" >= ?", t)
				} else {
					to = &t
					q = q.Where(column+" < ?", t)
				}
			}
		}
		if from != nil && to != nil && !from.Before(*to) {
			return nil, invalid("from must precede to")
		}
		filters := []string{"instance_id", "from", "to"}
		if admin {
			filters = append(filters, "user_id")
		}
		if table == "credit_ledger" && !admin {
			filters = append(filters, "event_type")
			if s := c.Query("event_type"); s != "" {
				if !in(s, "grant", "reserve", "settle", "release", "expire", "adjust", "refund") {
					return nil, invalid("invalid ledger event type")
				}
				q = q.Where("entry_type = ?", s)
			}
		}
		if table == "usage_sessions" {
			filters = append(filters, "status")
			if s := c.Query("status"); s != "" {
				if !in(s, "active", "ended") {
					return nil, invalid("invalid usage session status")
				}
				q = q.Where("status = ?", s)
			}
		}
		dto := publicItem
		switch table {
		case "usage_sessions":
			dto = func(r row) (row, error) { return sessionDTO(db, r) }
		case "usage_charges":
			dto = func(r row) (row, error) {
				out := public(r)
				out["calculation_snapshot"] = decoded(r["calculation_snapshot"])
				return out, nil
			}
		}
		return platform.ListMap(c, a, q, filters, dto)
	}))
}

// sessionDTO exposes economic versions as policy_versions IDs; usage sessions
// reference the legacy pricing source rows internally.
func sessionDTO(db *gorm.DB, r row) (row, error) {
	out := public(r)
	out["config_snapshot"] = decoded(r["config_snapshot"])
	var e error
	if out["pricing_version_id"], e = platform.PolicyID(db, "pricing", r["pricing_version_id"]); e != nil {
		return nil, e
	}
	if out["gpu_price_version_id"], e = platform.PolicyID(db, "gpu-pricing", r["gpu_price_version_id"]); e != nil {
		return nil, e
	}
	return out, nil
}
func accountDTO(db *gorm.DB, ac row) (row, error) {
	left, held, e := balances(db, platform.Uint(ac["id"]))
	if e != nil {
		return nil, e
	}
	debt, e := outstandingDebt(db, platform.Uint(ac["id"]))
	if e != nil {
		return nil, e
	}
	p, e := accountPolicy(db, ac)
	if e != nil {
		return nil, e
	}
	cycle, e := cycleAt(p, now())
	if e != nil {
		return nil, e
	}
	next := cycle.AddDate(0, 0, 1)
	var event row
	err := db.Table("credit_enforcements").Where("account_id = ? AND status IN ('grace','stopping','unknown')", ac["id"]).Take(&event).Error
	var enforcement any
	if err == nil {
		enforcement, e = eventDTO(db, event)
		if e != nil {
			return nil, e
		}
	} else if err != gorm.ErrRecordNotFound {
		return nil, err
	}
	return row{"id": sid(ac["id"]), "user_id": sid(ac["user_id"]), "revision": sid(ac["revision"]), "status": ac["status"], "settlement_pending": platform.Bool(ac["settlement_pending"]), "remaining_pt": money(left.Sub(debt)), "reserved_pt": money(held), "available_pt": money(left.Sub(held).Sub(debt)), "outstanding_debt_pt": money(debt), "next_grant_at": next.In(zone).Format(time.RFC3339Nano), "server_time": now().Format(time.RFC3339Nano), "policy_version_id": sid(p["policy_version_id"]), "enforcement": enforcement}, nil
}

func outstandingDebt(db *gorm.DB, accountID uint64) (decimal.Decimal, error) {
	var amount string
	err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Table("usage_charges").Select("COALESCE(SUM(expected_pt-settled_pt),0)").Where("account_id=? AND status='insufficient'", accountID).Scan(&amount).Error
	return dec(amount), err
}
