package billing

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type StopHook func(context.Context, *platform.App, *gorm.DB, uint64, uint64, bool, string) (row, error)

var stopHook StopHook

// SetStopHook is called once during registration, before workers start.
func SetStopHook(fn StopHook) { stopHook = fn }
func notify(db *gorm.DB, user uint64, kind, message, related string, id uint64) error {
	return db.Table("notifications").Create(&row{"user_id": user, "type": kind, "message": message, "related_type": related, "related_id": id}).Error
}
func hasPayableInsufficiency(charge row) bool {
	return platform.Text(charge["status"]) == "insufficient" && dec(charge["expected_pt"]).GreaterThan(dec(charge["settled_pt"]))
}
func detectExhaustion(db *gorm.DB, ac row, t time.Time) error {
	left, _, e := balances(db, platform.Uint(ac["id"]))
	if e != nil {
		return e
	}
	owed, e := outstandingDebt(db, platform.Uint(ac["id"]))
	if e != nil {
		return e
	}
	if left.Sub(owed).IsPositive() {
		return nil
	}
	// Only confirmed payable debt starts a shutdown clock, not a rounded-zero rate.
	var debt row
	if e = db.Table("usage_charges c").Select("COALESCE(SUM(c.expected_pt),0) expected_pt, COALESCE(SUM(c.settled_pt),0) settled_pt").Where("c.account_id = ? AND c.status = 'insufficient' AND "+chargeNotFailedSQL, ac["id"]).Take(&debt).Error; e != nil {
		return e
	}
	debt["status"] = "insufficient"
	if !hasPayableInsufficiency(debt) {
		return nil
	}
	instances, e := rows(db.Order("id"), "instances", "user_id = ? AND billing_mode = 'credit' AND product_type = 'elastic' AND creation_failed_at IS NULL AND deleted_at IS NULL AND (observed_state IN ('running','frozen','unknown') OR desired_state = 'running')", ac["user_id"])
	if e != nil {
		return e
	}
	for _, instance := range instances {
		// Historic events keep their membership, snapshot and deadline even when
		// their old wallet profile differs from the instance's frozen profile.
		var active int64
		if e = db.Table("credit_enforcements e").Joins("JOIN credit_enforcement_instances m ON m.enforcement_id=e.id").Where("e.account_id=? AND m.instance_id=? AND e.status IN ('grace','stopping','unknown')", ac["id"], instance["id"]).Count(&active).Error; e != nil {
			return e
		}
		if active > 0 {
			continue
		}
		if platform.Text(instance["desired_state"]) == "stopped" && platform.Text(instance["observed_state"]) == "running" {
			var previous row
			err := db.Table("credit_enforcements e").Select("e.*").Joins("JOIN credit_enforcement_instances m ON m.enforcement_id=e.id").Where("e.account_id=? AND e.status='completed' AND m.instance_id=?", ac["id"], instance["id"]).Order("e.id DESC").Take(&previous).Error
			if err != nil && err != gorm.ErrRecordNotFound {
				return err
			}
			if err == nil {
				proceed, err := CheckCreditStop(db.Statement.Context, db, platform.Uint(instance["id"]), platform.Uint(previous["id"]))
				if err != nil {
					return err
				}
				if proceed {
					continue
				}
			}
		}
		p, e := instanceCreditPolicy(db, instance)
		if e != nil {
			return e
		}
		var event row
		err := db.Table("credit_enforcements").Where("account_id=? AND policy_version_id=? AND status IN ('grace','stopping','unknown')", ac["id"], p["source_id"]).Take(&event).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return err
		}
		if err == gorm.ErrRecordNotFound {
			grace := platform.Uint(p["shutdown_grace_seconds"])
			if grace == 0 || grace > 86400 {
				return invalid("configured shutdown grace is required")
			}
			event, e = insert(db, "credit_enforcements", row{"account_id": ac["id"], "user_id": ac["user_id"], "policy_version_id": p["source_id"], "status": "grace", "exhausted_at": t, "shutdown_deadline": t.Add(time.Duration(grace) * time.Second), "policy_snapshot": jsonValue(p)})
			if e != nil {
				return e
			}
			n, e := insert(db, "notifications", row{"user_id": ac["user_id"], "type": "credit_exhausted", "message": fmt.Sprintf("Credit exhausted. Save your data and stop the affected elastic instances now. After %d seconds the platform will force-stop them. Unsaved memory and in-flight writes may be lost. Instances and disk data will NOT be deleted. Custom/admin-exempt instances are unaffected.", grace), "related_type": "credit_enforcement", "related_id": event["id"]})
			if e != nil {
				return e
			}
			if e = db.Table("credit_enforcements").Where("id=?", event["id"]).Update("notification_id", n["id"]).Error; e != nil {
				return e
			}
		}
		if e = db.Table("credit_enforcement_instances").Create(&row{"enforcement_id": event["id"], "instance_id": instance["id"], "status": "pending"}).Error; e != nil {
			return e
		}
	}
	return nil
}

// CheckCreditStop is the last durable gate immediately before an instance worker
// sends an agent stop. Its reason/event identity must never be discarded on retry.
func CheckCreditStop(ctx context.Context, db *gorm.DB, instanceID, eventID uint64) (bool, error) {
	db = db.WithContext(ctx)
	event, e := platform.Get(db, "credit_enforcements", eventID, false)
	if e != nil {
		return false, e
	}
	ac, e := platform.Get(db, "credit_accounts", platform.Uint(event["account_id"]), true)
	if e != nil {
		return false, e
	}
	if e = restoreEnforcement(db, ac, now()); e != nil {
		return false, e
	}
	event, e = platform.Get(db, "credit_enforcements", eventID, true)
	if e != nil {
		return false, e
	}
	instance, e := platform.Get(db, "instances", instanceID, true)
	if e != nil {
		return false, e
	}
	if platform.Text(instance["billing_mode"]) != "credit" || platform.Text(instance["product_type"]) != "elastic" || instance["creation_failed_at"] != nil || instance["deleted_at"] != nil || platform.Uint(instance["user_id"]) != platform.Uint(event["user_id"]) {
		return false, nil
	}
	var membership row
	if e = db.Table("credit_enforcement_instances").Clauses(clause.Locking{Strength: "UPDATE"}).Where("enforcement_id = ? AND instance_id = ?", eventID, instanceID).Take(&membership).Error; e == gorm.ErrRecordNotFound {
		return false, nil
	} else if e != nil {
		return false, e
	}
	at := now()
	if platform.Text(event["status"]) == "completed" {
		left, _, e := balances(db, platform.Uint(ac["id"]))
		if e != nil {
			return false, e
		}
		owed, e := outstandingDebt(db, platform.Uint(ac["id"]))
		if e != nil {
			return false, e
		}
		left = left.Sub(owed)
		var replacements int64
		if e = db.Table("credit_enforcements e").Joins("JOIN credit_enforcement_instances m ON m.enforcement_id=e.id").Where("e.account_id = ? AND e.id <> ? AND e.status IN ('grace','stopping','unknown') AND (e.policy_version_id = ? OR m.instance_id = ?)", ac["id"], eventID, event["policy_version_id"], instanceID).Count(&replacements).Error; e != nil {
			return false, e
		}
		if !canReactivateCompletedEvent(event, instance, money(left), replacements > 0, at) {
			return false, nil
		}
		if e = db.Table("credit_enforcements").Where("id = ? AND status = 'completed'", eventID).Updates(row{"status": "stopping", "revision": gorm.Expr("revision + 1")}).Error; e != nil {
			return false, e
		}
		event["status"] = "stopping"
	}
	if !in(platform.Text(event["status"]), "grace", "stopping", "unknown") || at.Before(stamp(event["shutdown_deadline"])) {
		return false, nil
	}
	if platform.Text(membership["status"]) == "stopped" && platform.Text(instance["observed_state"]) == "running" {
		if e = db.Table("credit_enforcement_instances").Where("id = ?", membership["id"]).Updates(row{"status": "pending", "task_id": nil, "revision": gorm.Expr("revision + 1")}).Error; e != nil {
			return false, e
		}
	}
	return true, nil
}
func canReactivateCompletedEvent(event, instance row, remaining string, hasReplacement bool, at time.Time) bool {
	observed := stamp(instance["observed_at"])
	deadline := stamp(event["shutdown_deadline"])
	completed := stamp(event["updated_at"])
	exhausted := stamp(event["exhausted_at"])
	return platform.Text(event["status"]) == "completed" && !hasReplacement && !dec(remaining).IsPositive() && platform.Text(instance["billing_mode"]) == "credit" && platform.Text(instance["product_type"]) == "elastic" && platform.Text(instance["desired_state"]) == "stopped" && platform.Text(instance["observed_state"]) == "running" && instance["deleted_at"] == nil && platform.Uint(instance["user_id"]) == platform.Uint(event["user_id"]) && !observed.IsZero() && !deadline.IsZero() && !completed.IsZero() && !exhausted.IsZero() && !at.Before(deadline) && !observed.Before(deadline) && observed.After(exhausted) && !observed.Before(completed) && !observed.After(at)
}
func eventDTO(db *gorm.DB, event row) (row, error) {
	out := public(event)
	pid, e := platform.PolicyID(db, "credit", event["policy_version_id"])
	if e != nil {
		return nil, e
	}
	out["policy_version_id"] = pid
	items, e := rows(db, "credit_enforcement_instances", "enforcement_id = ?", event["id"])
	if e != nil {
		return nil, e
	}
	ids := []string{}
	publicItems := []row{}
	for _, i := range items {
		ids = append(ids, sid(i["instance_id"]))
		publicItems = append(publicItems, public(i))
	}
	out["instance_ids"] = ids
	out["instances"] = publicItems
	var n row
	if event["notification_id"] != nil {
		n, e = platform.Get(db, "notifications", platform.Uint(event["notification_id"]), false)
		if e != nil {
			return nil, e
		}
		out["notification"] = row{"id": sid(n["id"]), "created_at": public(n)["created_at"], "read_at": public(n)["read_at"]}
	}
	return out, nil
}
func registerEnforcements(a *platform.App, g *gin.RouterGroup) {
	g.GET("/credit-enforcements", platform.Handle(func(c *gin.Context) (any, error) {
		db := platform.DB(c, a)
		q := db.Table("credit_enforcements")
		if s := c.Query("status"); s != "" {
			if !in(s, "grace", "stopping", "completed", "cancelled", "unknown") {
				return nil, invalid("invalid enforcement status")
			}
			q = q.Where("status = ?", s)
		}
		if s := c.Query("user_id"); s != "" {
			id, e := platform.IDString(s)
			if e != nil {
				return nil, e
			}
			q = q.Where("user_id = ?", id)
		}
		return platform.ListMap(c, a, q, []string{"status", "user_id"}, func(r row) (row, error) { return eventDTO(db, r) })
	}))
	g.GET("/credit-enforcements/:id", platform.Handle(func(c *gin.Context) (any, error) {
		id, e := requireID(c)
		if e != nil {
			return nil, e
		}
		r, e := platform.Get(platform.DB(c, a), "credit_enforcements", id, false)
		if e != nil {
			return nil, e
		}
		platform.SetETag(c, r)
		return eventDTO(platform.DB(c, a), r)
	}))
	g.POST("/credit-enforcements/:id/extend", platform.Handle(func(c *gin.Context) (any, error) {
		var b struct {
			Deadline string `json:"shutdown_deadline"`
		}
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		deadline, e := parseTime(b.Deadline)
		if e != nil {
			return nil, e
		}
		id, e := requireID(c)
		if e != nil {
			return nil, e
		}
		db := platform.DB(c, a)
		event, e := platform.Get(db, "credit_enforcements", id, true)
		if e != nil {
			return nil, e
		}
		if platform.Text(event["status"]) != "grace" || !deadline.After(stamp(event["shutdown_deadline"])) || !deadline.After(now()) {
			return nil, conflict("invalid_state", "only a pending grace deadline may be explicitly extended into the future")
		}
		event, e = platform.Save(c, a, "credit_enforcements", event, row{"shutdown_deadline": deadline})
		if e != nil {
			return nil, e
		}
		if e = platform.Audit(c.Request.Context(), db, platform.Current(c).ID, 0, 0, "credit.enforcement.extend", row{"enforcement_id": sid(event["id"]), "shutdown_deadline": deadline.Format(time.RFC3339Nano)}); e != nil {
			return nil, e
		}
		if e = notify(db, platform.Uint(event["user_id"]), "credit_deadline_extended", "An administrator extended your credit shutdown deadline to "+deadline.Format(time.RFC3339Nano)+". Save your data; force-stop never deletes disks.", "credit_enforcement", id); e != nil {
			return nil, e
		}
		platform.SetETag(c, event)
		return eventDTO(db, event)
	}))
	g.POST("/credit-enforcements/:id/reconcile", platform.Handle(func(c *gin.Context) (any, error) {
		var b struct{}
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		id, e := requireID(c)
		if e != nil {
			return nil, e
		}
		r, e := platform.Get(platform.DB(c, a), "credit_enforcements", id, true)
		if e != nil {
			return nil, e
		}
		if e = platform.Revision(c, r); e != nil {
			return nil, e
		}
		task, e := platform.Enqueue(platform.DB(c, a), "credit_enforcement_reconcile", platform.Uint(r["user_id"]), 0, 0, false, row{"enforcement_id": id})
		if e != nil {
			return nil, e
		}
		platform.Reply(c, 202, task)
		return nil, nil
	}))
}
func enforceAll(ctx context.Context, a *platform.App) error {
	// A completed stop can race a previously dispatched native start. Fresh
	// observations must reactivate the same expired event before normal dispatch.
	var late []struct {
		EventID    uint64
		InstanceID uint64
	}
	if e := a.DB.WithContext(ctx).Raw("SELECT e.id AS event_id,i.id AS instance_id FROM credit_enforcements e JOIN credit_enforcement_instances m ON m.enforcement_id=e.id JOIN instances i ON i.id=m.instance_id WHERE e.status='completed' AND i.product_type='elastic' AND i.billing_mode='credit' AND i.desired_state='stopped' AND i.observed_state='running' AND i.deleted_at IS NULL AND e.id=(SELECT MAX(newer.id) FROM credit_enforcements newer JOIN credit_enforcement_instances nm ON nm.enforcement_id=newer.id WHERE newer.account_id=e.account_id AND newer.status='completed' AND nm.instance_id=i.id)").Scan(&late).Error; e != nil {
		return e
	}
	for _, item := range late {
		if e := a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error { _, err := CheckCreditStop(ctx, db, item.InstanceID, item.EventID); return err }); e != nil {
			return e
		}
	}
	var ids []uint64
	if e := a.DB.WithContext(ctx).Table("credit_enforcements").Where("status IN ('grace','stopping','unknown')").Order("id").Pluck("id", &ids).Error; e != nil {
		return e
	}
	for _, id := range ids {
		if e := advanceEvent(ctx, a, id); e != nil {
			return e
		}
	}
	return nil
}
func advanceEvent(ctx context.Context, a *platform.App, id uint64) error {
	var members []uint64
	if e := a.DB.WithContext(ctx).Table("credit_enforcement_instances").Where("enforcement_id=?", id).Order("instance_id").Pluck("instance_id", &members).Error; e != nil {
		return e
	}
	unknown := false
	if len(members) == 0 {
		return advanceEventChunk(ctx, a, id, 0, 0, &unknown)
	}
	for start := 0; start < len(members); start += settlementChunkSize {
		end := start + settlementChunkSize
		if end > len(members) {
			end = len(members)
		}
		if e := advanceEventChunk(ctx, a, id, members[start], members[end-1], &unknown); e != nil {
			return e
		}
	}
	return nil
}

func advanceEventChunk(ctx context.Context, a *platform.App, id, first, last uint64, passUnknown *bool) error {
	return a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		event, e := platform.Get(db, "credit_enforcements", id, false)
		if e != nil {
			return e
		}
		ac, e := platform.Get(db, "credit_accounts", platform.Uint(event["account_id"]), true)
		if e != nil {
			return e
		}
		if e = restoreEnforcement(db, ac, now()); e != nil {
			return e
		}
		event, e = platform.Get(db, "credit_enforcements", id, true)
		if e != nil {
			return e
		}
		if !in(platform.Text(event["status"]), "grace", "stopping", "unknown") {
			return nil
		}
		items, e := rows(db.Clauses(clause.Locking{Strength: "UPDATE"}).Order("instance_id").Limit(settlementChunkSize), "credit_enforcement_instances", "enforcement_id = ? AND instance_id >= ? AND instance_id <= ?", id, first, last)
		if e != nil {
			return e
		}
		allStopped := true
		unknown := *passUnknown
		t := now()
		for _, item := range items {
			i, e := platform.Get(db, "instances", platform.Uint(item["instance_id"]), true)
			if e != nil {
				return e
			}
			exempt := platform.Text(i["billing_mode"]) != "credit" || platform.Text(i["product_type"]) != "elastic" || i["creation_failed_at"] != nil
			stopped := in(platform.Text(i["observed_state"]), "stopped", "deleted") && !stamp(i["observed_at"]).Before(stamp(event["exhausted_at"]))
			if exempt || stopped || platform.Text(item["status"]) == "stopped" {
				if platform.Text(item["status"]) != "stopped" {
					if e = db.Table("credit_enforcement_instances").Where("id = ?", item["id"]).Updates(row{"status": "stopped", "revision": gorm.Expr("revision + 1")}).Error; e != nil {
						return e
					}
				}
				continue
			}
			allStopped = false
			if t.Before(stamp(event["shutdown_deadline"])) {
				continue
			}
			if item["task_id"] != nil {
				task, e := platform.Get(db, "tasks", platform.Uint(item["task_id"]), false)
				if e != nil {
					return e
				}
				status := platform.Text(task["status"])
				if status == "unknown" {
					unknown = true
					var online int64
					if e = db.Table("nodes").Where("id = ? AND health = 'online'", i["node_id"]).Count(&online).Error; e != nil {
						return e
					}
					if online > 0 && stopHook != nil {
						if _, e = stopHook(ctx, a, db, platform.Uint(i["id"]), platform.Uint(i["user_id"]), true, "credit-enforcement:"+sid(event["id"])); e != nil {
							return e
						}
					}
					if e = notifyOnce(db, event, "credit_stop_pending", "A credit shutdown result is unknown. The platform retains resources and will reconcile the original stop when the node recovers. No disk is deleted."); e != nil {
						return e
					}
					continue
				}
				if in(status, "queued", "running", "waiting") {
					continue
				}
				if status == "succeeded" {
					unknown = true
					if e = queueEventReconcile(db, event); e != nil {
						return e
					}
					continue
				}
			}
			if stopHook == nil {
				return platform.E(503, "service_unavailable", "instance stop integration is not registered")
			}
			task, e := stopHook(ctx, a, db, platform.Uint(i["id"]), platform.Uint(i["user_id"]), true, "credit-enforcement:"+sid(event["id"]))
			if e != nil {
				unknown = true
				if e = notifyOnce(db, event, "credit_stop_pending", "A credit shutdown is pending because an instance operation or node is unavailable. No instance or disk is deleted."); e != nil {
					return e
				}
				continue
			}
			if e = db.Table("credit_enforcement_instances").Where("id = ?", item["id"]).Updates(row{"task_id": platform.Uint(task["task_id"]), "status": "queued", "revision": gorm.Expr("revision + 1")}).Error; e != nil {
				return e
			}
		}
		// A page cannot complete the event while another page has live members.
		var remaining int64
		if e = db.Table("credit_enforcement_instances").Where("enforcement_id=? AND status<>'stopped'", id).Count(&remaining).Error; e != nil {
			return e
		}
		allStopped = allStopped && remaining == 0
		status := "grace"
		if allStopped {
			status = "completed"
		} else if unknown {
			status = "unknown"
		} else if !t.Before(stamp(event["shutdown_deadline"])) {
			status = "stopping"
		}
		if status != platform.Text(event["status"]) {
			if e = db.Table("credit_enforcements").Where("id = ?", id).Updates(row{"status": status, "revision": gorm.Expr("revision + 1")}).Error; e != nil {
				return e
			}
		}
		*passUnknown = unknown
		return nil
	})
}
func notifyOnce(db *gorm.DB, event row, kind, message string) error {
	var n int64
	if e := db.Table("notifications").Where("related_type = 'credit_enforcement' AND related_id = ? AND type = ?", event["id"], kind).Count(&n).Error; e != nil {
		return e
	}
	if n > 0 {
		return nil
	}
	return notify(db, platform.Uint(event["user_id"]), kind, message, "credit_enforcement", platform.Uint(event["id"]))
}
func queueEventReconcile(db *gorm.DB, event row) error {
	var count int64
	if e := db.Table("tasks").Where("task_type = 'credit_enforcement_reconcile' AND status IN ('queued','running','waiting') AND JSON_EXTRACT(parameters_redacted, '$.enforcement_id') = ?", platform.Uint(event["id"])).Count(&count).Error; e != nil {
		return e
	}
	if count > 0 {
		return nil
	}
	_, e := platform.Enqueue(db, "credit_enforcement_reconcile", platform.Uint(event["user_id"]), 0, 0, false, row{"enforcement_id": platform.Uint(event["id"])})
	return e
}
func reconcileEvent(ctx context.Context, a *platform.App, id uint64) error {
	items, e := rows(a.DB.WithContext(ctx), "credit_enforcement_instances", "enforcement_id = ?", id)
	if e != nil {
		return e
	}
	for _, item := range items {
		i, e := platform.Get(a.DB.WithContext(ctx), "instances", platform.Uint(item["instance_id"]), false)
		if e != nil {
			return e
		}
		if platform.Text(i["billing_mode"]) != "credit" || platform.Text(i["product_type"]) != "elastic" {
			continue
		}
		data, e := a.Agent(ctx, platform.Uint(i["node_id"]), "GET", "/1.0/instances/"+url.PathEscape(platform.Text(i["incus_name"]))+"?project="+url.QueryEscape(platform.Text(i["project_name"])), nil)
		if e != nil {
			return platform.Unknown(e)
		}
		meta, ok := data["metadata"].(map[string]any)
		if !ok {
			return platform.Unknown(platform.E(502, "agent_invalid_response", "agent instance response is incomplete"))
		}
		cfg, ok := meta["config"].(map[string]any)
		if !ok || platform.Text(i["incus_uuid"]) == "" || !strings.EqualFold(platform.Text(cfg["volatile.uuid"]), platform.Text(i["incus_uuid"])) {
			return platform.Unknown(conflict("configuration_conflict", "instance identity cannot be established during credit reconciliation"))
		}
		status := platform.Text(meta["status"])
		if !strings.EqualFold(status, "Stopped") && !strings.EqualFold(status, "Running") {
			continue
		}
		if e = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
			if _, e := account(db, platform.Uint(i["user_id"])); e != nil {
				return e
			}
			instance, e := platform.Get(db, "instances", platform.Uint(i["id"]), true)
			if e != nil {
				return e
			}
			if strings.EqualFold(status, "Stopped") {
				if e = InstanceUsage(ctx, db, instance, "stopped", nil); e != nil {
					return e
				}
				if e = db.Table("instances").Where("id = ?", i["id"]).Updates(row{"observed_state": "stopped", "observed_at": now(), "revision": gorm.Expr("revision + 1")}).Error; e != nil {
					return e
				}
				return db.Table("credit_enforcement_instances").Where("id = ?", item["id"]).Updates(row{"status": "stopped", "revision": gorm.Expr("revision + 1")}).Error
			}
			if item["task_id"] != nil {
				task, e := platform.Get(db, "tasks", platform.Uint(item["task_id"]), false)
				if e != nil {
					return e
				}
				if in(platform.Text(task["status"]), "succeeded", "failed", "cancelled") {
					return db.Table("credit_enforcement_instances").Where("id = ?", item["id"]).Updates(row{"task_id": nil, "status": "pending", "revision": gorm.Expr("revision + 1")}).Error
				}
			}
			return nil
		}); e != nil {
			return e
		}
	}
	return advanceEvent(ctx, a, id)
}
