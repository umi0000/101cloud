package monitoring

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

var histRawFields = []string{"id", "instance_id", "node_id", "instance_state", "runtime_started_at", "sampled_at", "window_start_at", "window_end_at", "window_seconds", "cpu_sampled_at", "cpu_window_seconds", "cpu_usage_total_ns", "cpu_usage_delta_ns", "allocated_vcpus", "allocated_cpu_seconds", "cpu_utilization_pct", "memory_used_bytes", "memory_limit_bytes", "memory_peak_bytes", "gpu_state", "collection_complete", "metrics_complete", "attribution_complete", "reference_scan_quality", "error_code", "quality_reasons"}
var histGPUFields = []string{"id", "sample_id", "instance_id", "node_id", "sampled_at", "gpu_uuid", "gpu_model", "gpu_memory_total_bytes", "is_configured", "is_observed", "gpu_state", "memory_used_bytes", "utilization_pct", "process_count", "reference_count", "attribution_complete", "metrics_complete", "reference_scan_quality", "compute_process_support", "graphics_process_support", "utilization_support", "memory_support", "error_code", "quality_reasons"}
var histRollupSums = []string{"sample_count", "coverage_seconds", "unknown_count", "cpu_valid_seconds", "cpu_usage_delta_ns", "cpu_allocation_valid_seconds", "allocated_cpu_seconds", "cpu_normalized_usage_delta_ns", "cpu_utilization_pct_seconds", "memory_used_valid_seconds", "memory_used_byte_seconds", "memory_limit_valid_seconds", "memory_limit_byte_seconds", "memory_utilization_valid_seconds", "memory_utilization_pct_seconds"}
var histRollupPeaks = []string{"cpu_used_cores_peak", "cpu_utilization_peak_pct", "memory_utilization_peak_pct", "memory_peak_bytes"}
var histGPUSums = []string{"sample_count", "coverage_seconds", "unknown_count", "utilization_valid_seconds", "utilization_pct_seconds", "memory_valid_seconds", "memory_used_byte_seconds", "process_valid_sample_count"}
var histGPUPeaks = []string{"utilization_peak_pct", "memory_peak_bytes", "max_processes"}

func histProject(row platform.Row, fields []string) platform.Row {
	out := platform.Row{}
	for _, k := range fields {
		v := row[k]
		if v == nil {
			out[k] = nil
			continue
		}
		switch k {
		case "quality_reasons":
			var reasons []string
			if json.Unmarshal([]byte(platform.Text(v)), &reasons) == nil {
				out[k] = reasons
			} else {
				out[k] = []string{}
			}
		case "collection_complete", "metrics_complete", "attribution_complete", "is_configured", "is_observed":
			out[k] = platform.Bool(v)
		default:
			if strings.HasSuffix(k, "_at") {
				t := platform.Stamp(v)
				if t.IsZero() {
					out[k] = nil
				} else {
					out[k] = t.Format(time.RFC3339Nano)
				}
			} else {
				out[k] = platform.Text(v)
			}
		}
	}
	return out
}

func metricNumber(v any) any {
	if v == nil {
		return nil
	}
	d, ok := metricDecimal(v)
	if !ok {
		return nil
	}
	n, _ := d.Float64()
	return n
}
func metricCount(v any) any {
	if v == nil {
		return nil
	}
	return platform.Uint(v)
}
func rawMetricPoint(row platform.Row, cards []platform.Row) platform.Row {
	r := histProject(row, histRawFields)
	out := platform.Row{}
	for _, k := range []string{"id", "instance_id", "node_id", "instance_state", "runtime_started_at", "sampled_at", "window_start_at", "window_end_at", "window_seconds", "gpu_state", "collection_complete"} {
		out[k] = r[k]
	}
	out["quality"] = platform.Row{"complete": r["metrics_complete"], "attribution_complete": r["attribution_complete"], "reference_scan_quality": r["reference_scan_quality"], "reasons": r["quality_reasons"], "error_code": r["error_code"]}
	out["cpu"] = platform.Row{"collected_at": r["cpu_sampled_at"], "allocation_percent": metricNumber(row["cpu_utilization_pct"]), "usage_total_ns": r["cpu_usage_total_ns"], "delta_ns": r["cpu_usage_delta_ns"], "window_seconds": metricNumber(row["cpu_window_seconds"]), "allocated_vcpus": metricNumber(row["allocated_vcpus"]), "allocated_cpu_seconds": r["allocated_cpu_seconds"]}
	out["memory"] = platform.Row{"used_bytes": r["memory_used_bytes"], "limit_bytes": r["memory_limit_bytes"], "peak_bytes": r["memory_peak_bytes"]}
	gpus := make([]platform.Row, 0, len(cards))
	for _, card := range cards {
		g := histProject(card, histGPUFields)
		gpus = append(gpus, platform.Row{"uuid": g["gpu_uuid"], "model": g["gpu_model"], "memory_total_bytes": g["gpu_memory_total_bytes"], "configured": g["is_configured"], "observed": g["is_observed"], "state": g["gpu_state"], "process_count": metricCount(card["process_count"]), "referencing_process_count": metricCount(card["reference_count"]), "used_memory_bytes": g["memory_used_bytes"], "sm_utilization_percent": metricNumber(card["utilization_pct"]), "quality": platform.Row{"complete": g["metrics_complete"], "attribution_complete": g["attribution_complete"], "reference_scan_quality": g["reference_scan_quality"], "reasons": g["quality_reasons"], "error_code": g["error_code"]}})
	}
	out["gpus"] = gpus
	return out
}
func histQuery(c *gin.Context, allowed ...string) error {
	set := map[string]bool{}
	for _, k := range allowed {
		set[k] = true
	}
	for k, v := range c.Request.URL.Query() {
		if !set[k] || len(v) != 1 {
			return platform.E(400, "invalid_query", "Unsupported or repeated query parameter")
		}
	}
	return nil
}
func histTime(s string) (time.Time, error) {
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return t, platform.E(422, "invalid_time", "Expected RFC3339 timestamp with timezone")
	}
	return t, nil
}
func histOwned(c *gin.Context, a *platform.App) (platform.Row, error) {
	id, e := platform.ID(c, "id")
	if e != nil {
		return nil, e
	}
	return platform.Owned(c, a, "instances", id)
}
func RegisterHistory(a *platform.App, user, admin *gin.RouterGroup) {
	for _, g := range []*gin.RouterGroup{user, admin} {
		g.GET("/instances/:id/metrics/latest", platform.Handle(func(c *gin.Context) (any, error) { return histLatest(c, a) }))
		g.GET("/instances/:id/metrics/history", platform.Handle(func(c *gin.Context) (any, error) { return histHistory(c, a) }))
	}
	admin.GET("/audit-logs", platform.Handle(func(c *gin.Context) (any, error) { return histAudit(c, a) }))
}
func histLatest(c *gin.Context, a *platform.App) (any, error) {
	if e := histQuery(c); e != nil {
		return nil, e
	}
	instance, e := histOwned(c, a)
	if e != nil {
		return nil, e
	}
	var rows []platform.Row
	db := platform.DB(c, a)
	if e = db.Table("instance_metric_samples").Select(strings.Join(histRawFields, ",")).Where("instance_id = ?", instance["id"]).Order("sampled_at DESC,id DESC").Limit(1).Find(&rows).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	if len(rows) == 0 {
		return nil, platform.E(http.StatusServiceUnavailable, "metrics_unavailable", "No metric sample is available")
	}
	r := rows[0]
	var gpu []platform.Row
	if e = db.Table("instance_gpu_metric_samples").Select(strings.Join(histGPUFields, ",")).Where("sample_id = ?", r["id"]).Order("gpu_uuid").Find(&gpu).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	policy, e := rollupReadPolicy(c.Request.Context(), a)
	if e != nil {
		return nil, e
	}
	out := rawMetricPoint(r, gpu)
	age := a.Time().Sub(platform.Stamp(r["sampled_at"]))
	out["stale"] = age > time.Duration(policy.Interval)*2*time.Second
	out["age_seconds"] = decimal.NewFromInt(age.Microseconds()).Shift(-6).String()
	return out, nil
}
func histProjectMany(rows []platform.Row, fields []string) []platform.Row {
	out := make([]platform.Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, histProject(r, fields))
	}
	return out
}
func histAudit(c *gin.Context, a *platform.App) (any, error) {
	db := platform.DB(c, a).Table("audit_logs").Select("id,actor_user_id,task_id,event_type,resource_type,resource_id,request_id,result,occurred_at,created_at")
	filters := []string{"actor_user_id", "instance_id", "task_id", "from", "to"}
	for _, k := range []string{"actor_user_id", "instance_id", "task_id"} {
		if s := c.Query(k); s != "" {
			id, e := platform.IDString(s)
			if e != nil {
				return nil, e
			}
			if k == "instance_id" {
				db = db.Where("resource_type = 'instance' AND resource_id = ?", id)
			} else {
				db = db.Where(k+" = ?", id)
			}
		}
	}
	var from, to time.Time
	var e error
	if s := c.Query("from"); s != "" {
		from, e = histTime(s)
		if e != nil {
			return nil, e
		}
		db = db.Where("occurred_at >= ?", from.In(a.Location))
	}
	if s := c.Query("to"); s != "" {
		to, e = histTime(s)
		if e != nil {
			return nil, e
		}
		db = db.Where("occurred_at < ?", to.In(a.Location))
	}
	if !from.IsZero() && !to.IsZero() && !to.After(from) {
		return nil, platform.E(422, "invalid_range", "to must be after from")
	}
	return platform.List(c, a, db, filters)
}

type histCursor struct {
	Binding string    `json:"b"`
	At      time.Time `json:"t"`
	Key     uint64    `json:"k"`
}

func histBinding(c *gin.Context, from, to time.Time, res string, size int64) string {
	h := sha256.New()
	h.Write([]byte(strings.Join([]string{c.FullPath(), strconv.FormatUint(platform.Current(c).ID, 10), strconv.FormatBool(c.GetBool("admin")), c.Param("id"), from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), res, strconv.FormatInt(size, 10), c.Query("gpu_uuid")}, "\x00")))
	return hex.EncodeToString(h.Sum(nil))
}
func histDecodeCursor(a *platform.App, s, binding string) (histCursor, error) {
	var cursor histCursor
	bad := platform.E(400, "invalid_cursor", "Cursor is invalid or belongs to a different query")
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return cursor, bad
	}
	data, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return cursor, bad
	}
	sig, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return cursor, bad
	}
	mac := hmac.New(sha256.New, a.Config.MasterKey)
	mac.Write(data)
	if !hmac.Equal(mac.Sum(nil), sig) || json.Unmarshal(data, &cursor) != nil || cursor.Binding != binding || cursor.At.IsZero() {
		return histCursor{}, bad
	}
	return cursor, nil
}
func histEncodeCursor(a *platform.App, cursor histCursor) string {
	data, _ := json.Marshal(cursor)
	mac := hmac.New(sha256.New, a.Config.MasterKey)
	mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func histResolution(c *gin.Context, db *gorm.DB, id uint64, from, to time.Time) (string, int64, int64, error) {
	res := c.DefaultQuery("resolution", "auto")
	var target int64
	switch res {
	case "raw":
		return res, 0, 0, nil
	case "minute":
		target = 60
	case "hour":
		target = 3600
	case "day":
		target = 86400
	case "auto":
		span := to.Sub(from)
		if span <= 6*time.Hour {
			return "raw", 0, 0, nil
		}
		if span <= 7*24*time.Hour {
			target = 60
		} else if span <= 90*24*time.Hour {
			target = 3600
		} else {
			target = 86400
		}
	default:
		return "", 0, 0, platform.E(422, "unsupported_resolution", "Unsupported metric resolution")
	}
	var sizes []int64
	if e := db.Table("instance_metric_rollups").Where("instance_id = ? AND bucket_start_at < ? AND TIMESTAMPADD(SECOND,bucket_seconds,bucket_start_at) > ?", id, to, from).Distinct().Pluck("bucket_seconds", &sizes).Error; e != nil {
		return "", 0, 0, platform.WrapDB(e)
	}
	sort.Slice(sizes, func(i, j int) bool { return sizes[i] < sizes[j] })
	for _, s := range sizes {
		if s > 0 && s <= target && target%s == 0 {
			return histResolutionName(target), target, s, nil
		}
	}
	if res == "auto" {
		return "raw", 0, 0, nil
	}
	return histResolutionName(target), target, target, nil
}
func histResolutionName(n int64) string {
	switch n {
	case 60:
		return "minute"
	case 3600:
		return "hour"
	case 86400:
		return "day"
	}
	return strconv.FormatInt(n, 10)
}
func histBucketExpression(size int64) string {
	return "FROM_UNIXTIME(FLOOR((UNIX_TIMESTAMP(bucket_start_at)+28800)/" + strconv.FormatInt(size, 10) + ")*" + strconv.FormatInt(size, 10) + "-28800)"
}
func histCeil(t time.Time, size int64) time.Time {
	floor := rollupFloor(t, size)
	if floor.Equal(t) {
		return t
	}
	return floor.Add(time.Duration(size) * time.Second)
}
func histAggregateSelect(expr string, sums, peaks []string) string {
	cols := []string{expr + " AS bucket_start_at", "node_id"}
	for _, k := range sums {
		cols = append(cols, "SUM("+k+") AS "+k)
	}
	for _, k := range peaks {
		cols = append(cols, "MAX("+k+") AS "+k)
	}
	return strings.Join(cols, ",")
}
func histHistory(c *gin.Context, a *platform.App) (any, error) {
	if e := histQuery(c, "from", "to", "resolution", "gpu_uuid", "limit", "cursor"); e != nil {
		return nil, e
	}
	instance, e := histOwned(c, a)
	if e != nil {
		return nil, e
	}
	id := platform.Uint(instance["id"])
	from, e := histTime(c.Query("from"))
	if e != nil {
		return nil, e
	}
	to, e := histTime(c.Query("to"))
	if e != nil {
		return nil, e
	}
	from = from.In(a.Location)
	to = to.In(a.Location)
	if !to.After(from) {
		return nil, platform.E(422, "invalid_range", "to must be after from")
	}
	if len(c.Query("gpu_uuid")) > 128 {
		return nil, platform.E(422, "invalid_gpu_uuid", "GPU UUID is too long")
	}
	limit, e := platform.Limit(c, 100, 1000)
	if e != nil {
		return nil, e
	}
	db := platform.DB(c, a)
	res, size, source, e := histResolution(c, db, id, from, to)
	if e != nil {
		return nil, e
	}
	binding := histBinding(c, from, to, c.DefaultQuery("resolution", "auto")+":"+res+":"+strconv.FormatInt(source, 10), size)
	var cursor histCursor
	if s := c.Query("cursor"); s != "" {
		cursor, e = histDecodeCursor(a, s, binding)
		if e != nil {
			return nil, e
		}
	}
	var rows []platform.Row
	gpu := c.Query("gpu_uuid")
	if size == 0 {
		q := db.Table("instance_metric_samples").Select(strings.Join(histRawFields, ",")).Where("instance_id = ? AND sampled_at >= ? AND sampled_at < ?", id, from, to)
		if gpu != "" {
			q = q.Where("EXISTS (SELECT 1 FROM instance_gpu_metric_samples g WHERE g.sample_id=instance_metric_samples.id AND g.gpu_uuid = ?)", gpu)
		}
		if !cursor.At.IsZero() {
			q = q.Where("(sampled_at > ? OR (sampled_at = ? AND id > ?))", cursor.At.In(a.Location), cursor.At.In(a.Location), cursor.Key)
		}
		if e = q.Order("sampled_at,id").Limit(limit + 1).Find(&rows).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
	} else {
		expr := histBucketExpression(size)
		q := db.Table("instance_metric_rollups").Select(histAggregateSelect(expr, histRollupSums, histRollupPeaks)).Where("instance_id = ? AND bucket_seconds = ? AND bucket_start_at >= ? AND bucket_start_at < ?", id, source, rollupFloor(from, size), histCeil(to, size))
		if gpu != "" {
			q = q.Where("EXISTS (SELECT 1 FROM instance_gpu_metric_rollups g WHERE g.instance_id=instance_metric_rollups.instance_id AND g.node_id=instance_metric_rollups.node_id AND g.bucket_start_at=instance_metric_rollups.bucket_start_at AND g.bucket_seconds=instance_metric_rollups.bucket_seconds AND g.gpu_uuid = ?)", gpu)
		}
		q = q.Group(expr + ",node_id")
		if !cursor.At.IsZero() {
			q = q.Having("("+expr+" > ? OR ("+expr+" = ? AND node_id > ?))", cursor.At.In(a.Location), cursor.At.In(a.Location), cursor.Key)
		}
		if e = q.Order("bucket_start_at,node_id").Limit(limit + 1).Find(&rows).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
	}
	var next any
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		at, key := platform.Stamp(last["sampled_at"]), platform.Uint(last["id"])
		if size > 0 {
			at = platform.Stamp(last["bucket_start_at"])
			key = platform.Uint(last["node_id"])
		}
		next = histEncodeCursor(a, histCursor{binding, at, key})
	}
	items := make([]platform.Row, 0, len(rows))
	for _, r := range rows {
		if size == 0 {
			var cards []platform.Row
			q := db.Table("instance_gpu_metric_samples").Select(strings.Join(histGPUFields, ",")).Where("sample_id = ?", r["id"])
			if gpu != "" {
				q = q.Where("gpu_uuid = ?", gpu)
			}
			if e = q.Order("gpu_uuid").Find(&cards).Error; e != nil {
				return nil, platform.WrapDB(e)
			}
			out := rawMetricPoint(r, cards)
			items = append(items, out)
		} else {
			out, e := histRollupPoint(db, id, r, size, source, gpu)
			if e != nil {
				return nil, e
			}
			items = append(items, out)
		}
	}
	metadata, e := histCoverage(db, id, from, to, size, source, gpu)
	if e != nil {
		return nil, e
	}
	metadata["items"] = items
	metadata["next_cursor"] = next
	metadata["resolution"] = res
	metadata["bucket_seconds"] = size
	metadata["source_bucket_seconds"] = source
	metadata["bucket_boundary_mode"] = "whole_bucket"
	metadata["from"] = from.Format(time.RFC3339Nano)
	metadata["to"] = to.Format(time.RFC3339Nano)
	return metadata, nil
}
func histAverage(out platform.Row, sum, valid, name string) {
	s, ok := rollupDecimal(out[sum])
	v, vok := rollupDecimal(out[valid])
	if ok && vok && v.IsPositive() {
		out[name] = s.DivRound(v, 9).String()
	} else {
		out[name] = nil
	}
}
func histRollupPoint(db *gorm.DB, id uint64, r platform.Row, size, source int64, gpu string) (platform.Row, error) {
	fields := append([]string{"bucket_start_at", "node_id"}, histRollupSums...)
	fields = append(fields, histRollupPeaks...)
	out := histProject(r, fields)
	start := platform.Stamp(r["bucket_start_at"])
	end := start.Add(time.Duration(size) * time.Second)
	out["bucket_end_at"] = end.Format(time.RFC3339Nano)
	out["bucket_seconds"] = size
	histAverage(out, "cpu_usage_delta_ns", "cpu_valid_seconds", "cpu_used_nanocores_avg")
	histAverage(out, "cpu_utilization_pct_seconds", "cpu_allocation_valid_seconds", "cpu_utilization_avg_pct")
	histAverage(out, "memory_used_byte_seconds", "memory_used_valid_seconds", "memory_used_avg_bytes")
	histAverage(out, "memory_limit_byte_seconds", "memory_limit_valid_seconds", "memory_limit_avg_bytes")
	histAverage(out, "memory_utilization_pct_seconds", "memory_utilization_valid_seconds", "memory_utilization_avg_pct")
	q := db.Table("instance_gpu_metric_rollups").Select(histAggregateSelect("MIN(bucket_start_at)", histGPUSums, histGPUPeaks)+",gpu_uuid,MAX(gpu_model) AS gpu_model,MAX(gpu_memory_total_bytes) AS gpu_memory_total_bytes").Where("instance_id = ? AND node_id = ? AND bucket_seconds = ? AND bucket_start_at >= ? AND bucket_start_at < ?", id, r["node_id"], source, start, end).Group("node_id,gpu_uuid")
	if gpu != "" {
		q = q.Where("gpu_uuid = ?", gpu)
	}
	var cards []platform.Row
	if e := q.Order("gpu_uuid").Find(&cards).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	gpuFields := append([]string{"gpu_uuid", "gpu_model", "gpu_memory_total_bytes"}, histGPUSums...)
	gpuFields = append(gpuFields, histGPUPeaks...)
	gpus := histProjectMany(cards, gpuFields)
	for _, g := range gpus {
		histAverage(g, "utilization_pct_seconds", "utilization_valid_seconds", "utilization_avg_pct")
		histAverage(g, "memory_used_byte_seconds", "memory_valid_seconds", "memory_used_avg_bytes")
	}
	out["gpus"] = gpus
	return out, nil
}
func histCoverage(db *gorm.DB, id uint64, from, to time.Time, size, source int64, gpu string) (platform.Row, error) {
	ranges := platform.Row{}
	var rawRange platform.Row
	q := db.Table("instance_metric_samples").Select("MIN(sampled_at) AS from_at,MAX(sampled_at) AS to_at").Where("instance_id = ?", id)
	if gpu != "" {
		q = q.Where("EXISTS (SELECT 1 FROM instance_gpu_metric_samples g WHERE g.sample_id=instance_metric_samples.id AND g.gpu_uuid=?)", gpu)
	}
	if e := q.Take(&rawRange).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	ranges["raw"] = histProject(rawRange, []string{"from_at", "to_at"})
	var rollRanges []platform.Row
	rq := db.Table("instance_metric_rollups").Select("bucket_seconds,MIN(bucket_start_at) AS from_at,MAX(TIMESTAMPADD(SECOND,bucket_seconds,bucket_start_at)) AS to_at").Where("instance_id = ?", id)
	if gpu != "" {
		rq = rq.Where("EXISTS (SELECT 1 FROM instance_gpu_metric_rollups g WHERE g.instance_id=instance_metric_rollups.instance_id AND g.node_id=instance_metric_rollups.node_id AND g.bucket_start_at=instance_metric_rollups.bucket_start_at AND g.bucket_seconds=instance_metric_rollups.bucket_seconds AND g.gpu_uuid=?)", gpu)
	}
	if e := rq.Group("bucket_seconds").Order("bucket_seconds").Find(&rollRanges).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	ranges["rollups"] = histProjectMany(rollRanges, []string{"bucket_seconds", "from_at", "to_at"})
	intervals := []rollupInterval{}
	if size == 0 {
		var samples []platform.Row
		query := db.Table("instance_metric_samples").Select("id,window_start_at,window_end_at,cpu_sampled_at,cpu_window_seconds").Where("instance_id = ? AND ((window_end_at > ? AND window_start_at < ?) OR (cpu_sampled_at > ? AND TIMESTAMPADD(MICROSECOND,-CAST(cpu_window_seconds*1000000 AS SIGNED),cpu_sampled_at) < ?))", id, from, to, from, to)
		if gpu != "" {
			query = query.Where("EXISTS (SELECT 1 FROM instance_gpu_metric_samples g WHERE g.sample_id=instance_metric_samples.id AND g.gpu_uuid=?)", gpu)
		}
		if e := query.Find(&samples).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		for _, r := range samples {
			intervals = append(intervals, rollupClip(rollupInterval{platform.Stamp(r["window_start_at"]), platform.Stamp(r["window_end_at"])}, from, to))
			if gpu == "" {
				intervals = append(intervals, rollupClip(rollupCPUWindow(r), from, to))
			}
		}
	} else {
		var rows []platform.Row
		table := "instance_metric_rollups"
		if gpu != "" {
			table = "instance_gpu_metric_rollups"
		}
		q := db.Table(table).Select("coverage_intervals").Where("instance_id = ? AND bucket_seconds = ? AND bucket_start_at < ? AND TIMESTAMPADD(SECOND,bucket_seconds,bucket_start_at) > ?", id, source, to, from)
		if gpu != "" {
			q = q.Where("gpu_uuid = ?", gpu)
		}
		if e := q.Find(&rows).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		for _, r := range rows {
			var spans []rollupInterval
			if e := json.Unmarshal([]byte(platform.Text(r["coverage_intervals"])), &spans); e != nil {
				return nil, platform.E(500, "invalid_metric_coverage", "Stored rollup coverage is invalid")
			}
			for _, v := range spans {
				intervals = append(intervals, rollupClip(v, from, to))
			}
		}
	}
	intervals = rollupUnion(intervals)
	gaps := []platform.Row{}
	for _, gap := range rollupUncovered(rollupInterval{from, to}, intervals) {
		gaps = append(gaps, platform.Row{"from": gap.From.Format(time.RFC3339Nano), "to": gap.To.Format(time.RFC3339Nano), "reason": "no_retained_observation"})
	}
	var recorded []platform.Row
	if e := db.Table("metric_gaps").Select("id,node_id,from_at,to_at,reason").Where("from_at < ? AND to_at > ? AND node_id IN (SELECT node_id FROM instance_metric_samples WHERE instance_id = ? UNION SELECT node_id FROM instance_metric_rollups WHERE instance_id = ? UNION SELECT node_id FROM instances WHERE id = ?)", to, from, id, id, id).Order("from_at,id").Find(&recorded).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	covered := rollupDuration(intervals)
	duration := rollupSeconds(rollupInterval{from, to})
	return platform.Row{"available_range": ranges, "coverage": platform.Row{"seconds": covered.String(), "fraction": covered.DivRound(duration, 9).String(), "intervals": intervals}, "gaps": gaps, "collection_gaps": histProjectMany(recorded, []string{"id", "node_id", "from_at", "to_at", "reason"})}, nil
}
