package billing

import (
	"database/sql"
	"net/url"
	"strconv"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

const billingTimezone = "Asia/Shanghai"

type billingBreakdown struct {
	CPUPT    string `json:"cpu_pt"`
	MemoryPT string `json:"memory_pt"`
	DiskPT   string `json:"disk_pt"`
	GPUPT    string `json:"gpu_pt"`
}
type billingPeriodSummary struct {
	From        time.Time        `json:"from"`
	To          time.Time        `json:"to"`
	ConsumedPT  string           `json:"consumed_pt"`
	RecordedPT  string           `json:"recorded_pt"`
	EstimatedPT string           `json:"estimated_pt"`
	Breakdown   billingBreakdown `json:"breakdown"`
}
type billingOverviewDTO struct {
	AsOf     time.Time                       `json:"as_of"`
	Timezone string                          `json:"timezone"`
	Periods  map[string]billingPeriodSummary `json:"periods"`
	Current  billingCurrentDTO               `json:"current"`
	Forecast billingForecastDTO              `json:"forecast"`
	Balance  billingBalanceDTO               `json:"balance"`
	Partial  bool                            `json:"partial"`
	Reasons  []string                        `json:"reasons"`
}
type billingTrendItem struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	ConsumedPT  *string   `json:"consumed_pt"`
	RecordedPT  *string   `json:"recorded_pt"`
	EstimatedPT *string   `json:"estimated_pt"`
	Partial     bool      `json:"partial"`
}
type billingTrendDTO struct {
	AsOf        time.Time          `json:"as_of"`
	Timezone    string             `json:"timezone"`
	Period      string             `json:"period"`
	From        time.Time          `json:"from"`
	To          time.Time          `json:"to"`
	InstanceID  *string            `json:"instance_id"`
	Granularity string             `json:"granularity"`
	Items       []billingTrendItem `json:"items"`
	Partial     bool               `json:"partial"`
	Reasons     []string           `json:"reasons"`
}
type billingInstanceDTO struct {
	InstanceID      string           `json:"instance_id"`
	Name            string           `json:"name"`
	Deleted         bool             `json:"deleted"`
	InstanceState   string           `json:"instance_state"`
	ConsumedPT      string           `json:"consumed_pt"`
	RecordedPT      string           `json:"recorded_pt"`
	EstimatedPT     string           `json:"estimated_pt"`
	Breakdown       billingBreakdown `json:"breakdown"`
	CurrentHourlyPT *string          `json:"current_hourly_pt"`
	RateStatus      string           `json:"rate_status"`
}
type billingInstancesDTO struct {
	AsOf     time.Time            `json:"as_of"`
	Timezone string               `json:"timezone"`
	Period   string               `json:"period"`
	From     time.Time            `json:"from"`
	To       time.Time            `json:"to"`
	Page     int                  `json:"page"`
	PageSize int                  `json:"page_size"`
	Total    int64                `json:"total"`
	Items    []billingInstanceDTO `json:"items"`
	Partial  bool                 `json:"partial"`
	Reasons  []string             `json:"reasons"`
}
type billingReportOptions struct {
	period, search string
	instance       uint64
	page, size     int
}
type billingWindow struct{ From, To time.Time }

func billingPeriod(at time.Time, period string) billingWindow {
	at = at.In(zone)
	d := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, zone)
	switch period {
	case "today":
		return billingWindow{d, d.AddDate(0, 0, 1)}
	case "week":
		d = d.AddDate(0, 0, -(int(d.Weekday())+6)%7)
		return billingWindow{d, d.AddDate(0, 0, 7)}
	case "month":
		d = time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, zone)
		return billingWindow{d, d.AddDate(0, 1, 0)}
	default:
		d = time.Date(at.Year(), 1, 1, 0, 0, 0, 0, zone)
		return billingWindow{d, d.AddDate(1, 0, 0)}
	}
}
func billingAxis(w billingWindow, period string) ([]billingWindow, string) {
	granularity := "day"
	if period == "today" {
		granularity = "hour"
	} else if period == "year" {
		granularity = "month"
	}
	items := make([]billingWindow, 0, 31)
	for from := w.From; from.Before(w.To); {
		to := from.AddDate(0, 0, 1)
		if granularity == "hour" {
			to = from.Add(time.Hour)
		} else if granularity == "month" {
			to = from.AddDate(0, 1, 0)
		}
		items = append(items, billingWindow{from, to})
		from = to
	}
	return items, granularity
}
func billingReportParams(c *gin.Context, kind string) (billingReportOptions, error) {
	o := billingReportOptions{page: 1, size: 20}
	q, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		return o, invalid("invalid query parameters")
	}
	for key, values := range q {
		allowed := kind != "overview" && (key == "period" || key == "instance_id")
		allowed = allowed || kind == "instances" && in(key, "search", "page", "page_size")
		if !allowed || len(values) != 1 {
			return o, invalid("unknown or duplicate query parameter: " + key)
		}
	}
	if kind == "overview" {
		return o, nil
	}
	o.period = q.Get("period")
	if !in(o.period, "today", "week", "month", "year") {
		return o, invalid("period must be today, week, month or year")
	}
	if values, ok := q["instance_id"]; ok {
		o.instance, err = platform.IDString(values[0])
		if err != nil {
			return o, err
		}
	}
	if kind == "instances" {
		o.search = strings.TrimSpace(q.Get("search"))
		if len([]rune(o.search)) > 128 {
			return o, invalid("search must be at most 128 characters")
		}
		for key, dest := range map[string]*int{"page": &o.page, "page_size": &o.size} {
			if values, ok := q[key]; ok {
				v := values[0]
				if v == "" || strings.Trim(v, "0123456789") != "" {
					return o, invalid(key + " must be a positive integer")
				}
				n, e := strconv.ParseUint(v, 10, 31)
				if e != nil || n == 0 || key == "page_size" && n > 100 {
					return o, invalid(key + " is out of range")
				}
				*dest = int(n)
			}
		}
	}
	return o, nil
}

func registerBillingReports(a *platform.App, user *gin.RouterGroup) {
	registerBillingInstanceOptions(a, user)
	registerBillingDistribution(a, user)
	for _, kind := range []string{"overview", "trend", "instances"} {
		kind := kind
		user.GET("/billing/"+kind, platform.Handle(func(c *gin.Context) (any, error) {
			o, err := billingReportParams(c, kind)
			if err != nil {
				return nil, err
			}
			var result any
			err = platform.DB(c, a).Transaction(func(db *gorm.DB) error {
				// First consistent read establishes the snapshot; no account()/FOR UPDATE.
				ac, e := find(db, "credit_accounts", "user_id = ?", platform.Current(c).ID)
				if he, ok := e.(*platform.HTTPError); ok && he.Status == 404 {
					return conflict("policy_not_configured", "an explicit credit allowance account is required")
				}
				if e != nil {
					return e
				}
				// Read the clock after establishing the consistent snapshot. Every
				// normally metered charge visible in it then ends before as_of.
				at := now()
				if o.instance != 0 {
					if _, e = find(db, "instances", "id = ? AND user_id = ?", o.instance, platform.Current(c).ID); e != nil {
						return e
					}
				}
				var live billingLive
				switch kind {
				case "overview":
					live, e = loadBillingLive(db, ac, at)
				case "trend":
					live, e = loadBillingUsage(db, ac, at, false)
				case "instances":
					live, e = loadBillingUsage(db, ac, at, true)
				}
				if e != nil {
					return e
				}
				if live.Reasons == nil {
					live.Reasons = []string{}
				}
				switch kind {
				case "overview":
					result, e = billingOverview(db, ac, at, live)
				case "trend":
					result, e = billingTrend(db, ac, at, live, o)
				case "instances":
					result, e = billingInstances(db, ac, at, live, o)
				}
				return e
			}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
			return result, err
		}))
	}
}

func billingOverview(db *gorm.DB, ac row, at time.Time, live billingLive) (billingOverviewDTO, error) {
	out := billingOverviewDTO{AsOf: at, Timezone: billingTimezone, Periods: map[string]billingPeriodSummary{}, Current: live.Current, Forecast: live.Forecast, Balance: live.Balance, Partial: live.Partial, Reasons: live.Reasons}
	periods := []string{"today", "week", "month", "year"}
	windows := make([]billingWindow, len(periods))
	for i, p := range periods {
		windows[i] = billingPeriod(at, p)
		windows[i].To = at
	}
	query, args := billingAggregateSQL(ac, windows, live.Tails, 0, false)
	var totals []billingAggregate
	if err := db.Raw(query, args...).Scan(&totals).Error; err != nil {
		return out, err
	}
	byWindow := make(map[int]billingAggregate, len(totals))
	for _, v := range totals {
		byWindow[v.WindowID] = v
	}
	for i, p := range periods {
		v := byWindow[i]
		out.Periods[p] = billingPeriodSummary{From: windows[i].From, To: at, ConsumedPT: money(v.consumed()), RecordedPT: money(dec(v.Recorded)), EstimatedPT: money(dec(v.Estimated)), Breakdown: v.breakdown()}
	}
	return out, nil
}
func billingTrend(db *gorm.DB, ac row, at time.Time, live billingLive, o billingReportOptions) (billingTrendDTO, error) {
	w := billingPeriod(at, o.period)
	axis, granularity := billingAxis(w, o.period)
	out := billingTrendDTO{AsOf: at, Timezone: billingTimezone, Period: o.period, From: w.From, To: w.To, Granularity: granularity, Items: make([]billingTrendItem, 0, len(axis)), Partial: live.Partial, Reasons: live.Reasons}
	if o.instance != 0 {
		id := sid(o.instance)
		out.InstanceID = &id
	}
	clipped := make([]billingWindow, len(axis))
	copy(clipped, axis)
	for i := range clipped {
		if clipped[i].To.After(at) {
			clipped[i].To = at
		}
	}
	query, args := billingAggregateSQL(ac, clipped, live.Tails, o.instance, false)
	var totals []billingAggregate
	if err := db.Raw(query, args...).Scan(&totals).Error; err != nil {
		return out, err
	}
	byWindow := make(map[int]billingAggregate, len(totals))
	for _, v := range totals {
		byWindow[v.WindowID] = v
	}
	for i, bucket := range axis {
		item := billingTrendItem{From: bucket.From, To: bucket.To}
		if bucket.From.Before(at) {
			v := byWindow[i]
			recorded, estimated, consumed := money(dec(v.Recorded)), money(dec(v.Estimated)), money(v.consumed())
			item.RecordedPT, item.EstimatedPT, item.ConsumedPT = &recorded, &estimated, &consumed
			item.Partial = bucket.To.After(at) || live.Partial
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

func billingInstances(db *gorm.DB, ac row, at time.Time, live billingLive, o billingReportOptions) (billingInstancesDTO, error) {
	w := billingPeriod(at, o.period)
	out := billingInstancesDTO{AsOf: at, Timezone: billingTimezone, Period: o.period, From: w.From, To: at, Page: o.page, PageSize: o.size, Items: []billingInstanceDTO{}, Partial: live.Partial, Reasons: live.Reasons}
	filter := "i.user_id = ?"
	filterArgs := []any{ac["user_id"]}
	if o.instance != 0 {
		filter += " AND i.id = ?"
		filterArgs = append(filterArgs, o.instance)
	}
	if o.search != "" {
		// LOCATE treats '%' and '_' literally, unlike unescaped LIKE.
		filter += " AND (LOCATE(?,i.logical_name)>0 OR CAST(i.id AS CHAR)=?)"
		filterArgs = append(filterArgs, o.search, o.search)
	}
	if err := db.Table("instances i").Where(filter, filterArgs...).Count(&out.Total).Error; err != nil {
		return out, err
	}
	query, args := billingAggregateSQL(ac, []billingWindow{{w.From, at}}, live.Tails, o.instance, true)
	query = "WITH report_totals AS (" + query + ") SELECT i.id AS instance_id,i.logical_name AS name,i.deleted_at IS NOT NULL AS deleted,i.observed_state AS instance_state,COALESCE(t.recorded,0) AS recorded,COALESCE(t.estimated,0) AS estimated,COALESCE(t.cpu,0) AS cpu,COALESCE(t.memory,0) AS memory,COALESCE(t.disk,0) AS disk,COALESCE(t.gpu,0) AS gpu FROM instances i LEFT JOIN report_totals t ON t.instance_id=i.id WHERE " + filter + " ORDER BY COALESCE(t.recorded,0)+COALESCE(t.estimated,0) DESC,i.id DESC LIMIT ? OFFSET ?"
	args = append(args, filterArgs...)
	args = append(args, o.size, int64(o.page-1)*int64(o.size))
	var totals []struct {
		Aggregate     billingAggregate `gorm:"embedded"`
		Name          string
		Deleted       bool
		InstanceState string
	}
	if err := db.Raw(query, args...).Scan(&totals).Error; err != nil {
		return out, err
	}
	for _, v := range totals {
		total := v.Aggregate
		item := billingInstanceDTO{InstanceID: sid(total.InstanceID), Name: v.Name, Deleted: v.Deleted, InstanceState: v.InstanceState, ConsumedPT: money(total.consumed()), RecordedPT: money(dec(total.Recorded)), EstimatedPT: money(dec(total.Estimated)), Breakdown: total.breakdown(), RateStatus: "unavailable"}
		if rate, ok := live.Rates[total.InstanceID]; ok && rate.Status == "ready" {
			value := money(rate.Hourly)
			item.CurrentHourlyPT = &value
			item.RateStatus = "ready"
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Keep conversion exact even for aggregate values larger than a single charge.
type billingAggregate struct {
	WindowID                                    int
	InstanceID                                  uint64
	Recorded, Estimated, CPU, Memory, Disk, GPU string
}

func (a billingAggregate) consumed() decimal.Decimal { return dec(a.Recorded).Add(dec(a.Estimated)) }
func (a billingAggregate) breakdown() billingBreakdown {
	return billingBreakdown{money(dec(a.CPU)), money(dec(a.Memory)), money(dec(a.Disk)), money(dec(a.GPU))}
}
