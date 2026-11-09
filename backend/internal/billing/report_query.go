package billing

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
)

// Full days use daily sums; remaining complete hours use hourly sums. A
// monotonic source-end watermark proves when a trailing partial hour is safe;
// other partial boundaries and the unprocessed complement read exact raw rows.
func billingAggregateSQL(ac row, windows []billingWindow, tails []billingTail, instance uint64, byInstance bool) (string, []any) {
	args := make([]any, 0, len(windows)*24)
	rawParts, summaryParts := []string{}, []string{}
	type reportRange struct {
		window         int
		from, to       time.Time
		daily, partial bool
	}
	summaries := []reportRange{}
	addRaw := func(id int, w billingWindow, complete bool) {
		if !w.From.Before(w.To) {
			return
		}
		part := `SELECT w.window_id,w.from_at AS window_from_at,w.to_at AS window_to_at,
 c.from_at,c.to_at,c.expected_pt,s.instance_id,s.charge_kind,
 s.cpu_hourly_pt*CAST(COALESCE(JSON_UNQUOTE(JSON_EXTRACT(s.config_snapshot,'$.vcpus')),'0') AS DECIMAL(20,0))*1073741824 AS cpu_weight,
 s.memory_gib_hourly_pt*CAST(COALESCE(JSON_UNQUOTE(JSON_EXTRACT(s.config_snapshot,'$.memory_bytes')),'0') AS DECIMAL(20,0)) AS memory_weight
FROM (SELECT ? AS window_id,CAST(? AS DATETIME(6)) AS from_at,CAST(? AS DATETIME(6)) AS to_at) w
JOIN usage_charges c FORCE INDEX (ix_usage_charges_rollup) ON c.account_id=? AND c.report_rollup_complete=? AND c.to_at>? AND c.from_at<?
JOIN usage_sessions s ON s.id=c.session_id AND s.account_id=c.account_id
JOIN instances i ON i.id=s.instance_id AND i.user_id=s.user_id
WHERE s.user_id=? AND c.status IN ('pending','settled','insufficient')
AND s.billing_mode='credit' AND i.creation_failed_at IS NULL`
		args = append(args, id, w.From, w.To, ac["id"], complete, w.From, w.To, ac["user_id"])
		if complete && w.From.Equal(billingReportFloor(w.From, false)) && w.To.Before(w.From.Add(time.Hour)) {
			part += " AND NOT EXISTS (SELECT 1 FROM billing_report_state m WHERE m.account_id=? AND m.max_to_at<=?)"
			args = append(args, ac["id"], w.To)
			summaries = append(summaries, reportRange{id, w.From, w.To, false, true})
		}
		if instance != 0 {
			part += " AND s.instance_id=?"
			args = append(args, instance)
		}
		rawParts = append(rawParts, part)
	}
	addHours := func(id int, w billingWindow) {
		if !w.From.Before(w.To) {
			return
		}
		from, to := billingReportFloor(w.From, false), billingReportFloor(w.To, false)
		if from.Before(w.From) {
			from = from.Add(time.Hour)
		}
		if !from.Before(to) {
			addRaw(id, w, true)
			return
		}
		addRaw(id, billingWindow{w.From, from}, true)
		summaries = append(summaries, reportRange{id, from, to, false, false})
		addRaw(id, billingWindow{to, w.To}, true)
	}
	for id, w := range windows {
		if !w.From.Before(w.To) {
			continue
		}
		addRaw(id, w, false)
		from, to := billingReportFloor(w.From, true), billingReportFloor(w.To, true)
		if from.Before(w.From) {
			from = from.AddDate(0, 0, 1)
		}
		if !from.Before(to) {
			addHours(id, w)
			continue
		}
		addHours(id, billingWindow{w.From, from})
		summaries = append(summaries, reportRange{id, from, to, true, false})
		addHours(id, billingWindow{to, w.To})
	}
	// No elapsed windows is still a valid, typed empty query.
	if len(rawParts) == 0 {
		rawParts = append(rawParts, `SELECT 0 AS window_id,CAST(NULL AS DATETIME(6)) AS window_from_at,CAST(NULL AS DATETIME(6)) AS window_to_at,CAST(NULL AS DATETIME(6)) AS from_at,CAST(NULL AS DATETIME(6)) AS to_at,0 AS expected_pt,0 AS instance_id,'' AS charge_kind,0 AS cpu_weight,0 AS memory_weight WHERE FALSE`)
	}
	roundRatio := func(n, d string) string {
		return "((" + n + "-MOD(" + n + "," + d + "))/" + d + "+CASE WHEN MOD(" + n + "," + d + ")*2>=" + d + " THEN 1 ELSE 0 END)"
	}
	prefix := func(bound string) string {
		return roundRatio("c.expected_pt*1000000*TIMESTAMPDIFF(MICROSECOND,c.from_at,"+bound+")", "TIMESTAMPDIFF(MICROSECOND,c.from_at,c.to_at)")
	}
	lo, hi := prefix("GREATEST(c.from_at,c.window_from_at)"), prefix("LEAST(c.to_at,c.window_to_at)")
	query := "WITH report_sources AS (" + strings.Join(rawParts, " UNION ALL ") + `), report_prefixes AS (
SELECT c.window_id,c.instance_id,c.charge_kind,` + lo + ` AS lo_micro,` + hi + ` AS hi_micro,c.cpu_weight,c.memory_weight FROM report_sources c
), report_components AS (
SELECT window_id,instance_id,charge_kind,(hi_micro-lo_micro)/1000000 AS recorded,
CASE WHEN charge_kind='base_compute' AND cpu_weight+memory_weight>0 THEN
(` + roundRatio("hi_micro*cpu_weight", "(cpu_weight+memory_weight)") + `-` + roundRatio("lo_micro*cpu_weight", "(cpu_weight+memory_weight)") + `)/1000000 ELSE 0 END AS cpu FROM report_prefixes
), report_facts AS (
SELECT window_id,instance_id,recorded,CAST(0 AS DECIMAL(40,6)) AS estimated,cpu,
CASE WHEN charge_kind='base_compute' THEN recorded-cpu ELSE 0 END AS memory,
CASE WHEN charge_kind='storage' THEN recorded ELSE 0 END AS disk,
CASE WHEN charge_kind='gpu' THEN recorded ELSE 0 END AS gpu FROM report_components`
	for _, r := range summaries {
		table := "billing_report_hours"
		if r.daily {
			table = "billing_report_days"
		}
		part := "SELECT ?,r.instance_id,r.recorded,0,r.cpu,r.memory,r.disk,r.gpu FROM " + table + " r JOIN instances i ON i.id=r.instance_id WHERE r.account_id=? AND r.bucket_start>=? AND r.bucket_start<? AND i.user_id=? AND i.creation_failed_at IS NULL"
		args = append(args, r.window, ac["id"], r.from, r.to, ac["user_id"])
		if r.partial {
			part += " AND EXISTS (SELECT 1 FROM billing_report_state m WHERE m.account_id=? AND m.max_to_at<=?)"
			args = append(args, ac["id"], r.to)
		}
		if instance != 0 {
			part += " AND r.instance_id=?"
			args = append(args, instance)
		}
		summaryParts = append(summaryParts, part)
	}
	if len(summaryParts) > 0 {
		query += " UNION ALL " + strings.Join(summaryParts, " UNION ALL ")
	}
	// Aggregate tails per instance/window before binding, rather than emitting a
	// SQL placeholder row for every session or raw historical interval.
	type tailKey struct {
		window   int
		instance uint64
	}
	tailTotals := make(map[tailKey]billingTailAmounts)
	for _, tail := range tails {
		id := platform.Uint(tail.Session["instance_id"])
		if instance != 0 && id != instance {
			continue
		}
		for i, w := range windows {
			v := billingTailClip(tail, w)
			if v.total.IsZero() {
				continue
			}
			key := tailKey{i, id}
			previous := tailTotals[key]
			previous.total = previous.total.Add(v.total)
			previous.cpu = previous.cpu.Add(v.cpu)
			previous.memory = previous.memory.Add(v.memory)
			previous.disk = previous.disk.Add(v.disk)
			previous.gpu = previous.gpu.Add(v.gpu)
			tailTotals[key] = previous
		}
	}
	// Iterate deterministically (calendar, original live session order).
	seen := make(map[tailKey]bool, len(tailTotals))
	for i := range windows {
		for _, tail := range tails {
			key := tailKey{i, platform.Uint(tail.Session["instance_id"])}
			v, ok := tailTotals[key]
			if !ok || seen[key] {
				continue
			}
			seen[key] = true
			query += " UNION ALL SELECT ?,?,0,CAST(? AS DECIMAL(40,6)),CAST(? AS DECIMAL(40,6)),CAST(? AS DECIMAL(40,6)),CAST(? AS DECIMAL(40,6)),CAST(? AS DECIMAL(40,6))"
			args = append(args, key.window, key.instance, money(v.total), money(v.cpu), money(v.memory), money(v.disk), money(v.gpu))
		}
	}
	query += ") SELECT window_id,"
	group := "window_id"
	if byInstance {
		query += "instance_id,"
		group += ",instance_id"
	} else {
		query += "0 AS instance_id,"
	}
	query += "CAST(SUM(recorded) AS DECIMAL(40,6)) AS recorded,CAST(SUM(estimated) AS DECIMAL(40,6)) AS estimated,CAST(SUM(cpu) AS DECIMAL(40,6)) AS cpu,CAST(SUM(memory) AS DECIMAL(40,6)) AS memory,CAST(SUM(disk) AS DECIMAL(40,6)) AS disk,CAST(SUM(gpu) AS DECIMAL(40,6)) AS gpu FROM report_facts GROUP BY " + group
	return query, args
}

type billingTailAmounts struct{ total, cpu, memory, disk, gpu decimal.Decimal }

func billingTailClip(t billingTail, w billingWindow) billingTailAmounts {
	var out billingTailAmounts
	from, to := t.From, t.To
	if w.From.After(from) {
		from = w.From
	}
	if w.To.Before(to) {
		to = w.To
	}
	if !from.Before(to) {
		return out
	}
	lo := expected(t.Session, t.BaseMicros+uint64(from.Sub(t.From)/time.Microsecond))
	hi := expected(t.Session, t.BaseMicros+uint64(to.Sub(t.From)/time.Microsecond))
	out.total = hi.Sub(lo)
	switch platform.Text(t.Session["charge_kind"]) {
	case "storage":
		out.disk = out.total
	case "gpu":
		out.gpu = out.total
	case "base_compute":
		var cfg row
		_ = json.Unmarshal([]byte(platform.Text(t.Session["config_snapshot"])), &cfg)
		cpu := dec(t.Session["cpu_hourly_pt"]).Mul(decimal.NewFromInt(int64(platform.Uint(cfg["vcpus"])))).Mul(decimal.NewFromInt(1073741824))
		memory := dec(t.Session["memory_gib_hourly_pt"]).Mul(dec(sid(cfg["memory_bytes"])))
		weight := cpu.Add(memory)
		if weight.IsPositive() {
			out.cpu = hi.Mul(cpu).DivRound(weight, 6).Sub(lo.Mul(cpu).DivRound(weight, 6))
		}
		out.memory = out.total.Sub(out.cpu)
	default:
		panic(fmt.Sprintf("unsupported report tail charge kind %q", platform.Text(t.Session["charge_kind"])))
	}
	return out
}
