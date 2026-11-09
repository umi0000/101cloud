package billing

import (
	"database/sql"
	"net/url"
	"strconv"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type billingEventDTO struct {
	ID                string    `json:"id"`
	EventType         string    `json:"event_type"`
	OccurredAt        time.Time `json:"occurred_at"`
	InstanceID        *string   `json:"instance_id"`
	InstanceName      *string   `json:"instance_name"`
	GrantSource       *string   `json:"grant_source"`
	AmountPT          *string   `json:"amount_pt"`
	AvailableBeforePT *string   `json:"available_before_pt"`
	AvailableAfterPT  *string   `json:"available_after_pt"`
	HourlyBeforePT    *string   `json:"hourly_before_pt"`
	HourlyAfterPT     *string   `json:"hourly_after_pt"`
}

type billingEventsDTO struct {
	AsOf     time.Time         `json:"as_of"`
	Timezone string            `json:"timezone"`
	Period   string            `json:"period"`
	From     time.Time         `json:"from"`
	To       time.Time         `json:"to"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	Total    int64             `json:"total"`
	Items    []billingEventDTO `json:"items"`
}

func billingEventsParams(c *gin.Context) (billingReportOptions, error) {
	o := billingReportOptions{page: 1, size: 20}
	q, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		return o, invalid("invalid query parameters")
	}
	for key, values := range q {
		if !in(key, "period", "page", "page_size") || len(values) != 1 {
			return o, invalid("unknown or duplicate query parameter: " + key)
		}
	}
	o.period = q.Get("period")
	if !in(o.period, "today", "week", "month", "year") {
		return o, invalid("period must be today, week, month or year")
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
	return o, nil
}

func registerBillingEvents(a *platform.App, user *gin.RouterGroup) {
	user.GET("/billing/events", platform.Handle(func(c *gin.Context) (any, error) {
		o, err := billingEventsParams(c)
		if err != nil {
			return nil, err
		}
		var result billingEventsDTO
		err = platform.DB(c, a).Transaction(func(db *gorm.DB) error {
			// A plain identity read establishes the snapshot without financial locks.
			var owner struct{ ID uint64 }
			if e := db.Table("users").Select("id").Where("id = ?", platform.Current(c).ID).Take(&owner).Error; e != nil {
				return platform.WrapDB(e)
			}
			var e error
			result, e = billingEvents(db, platform.Current(c).ID, now(), o)
			return e
		}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		return result, err
	}))
}

func billingEvents(db *gorm.DB, userID uint64, at time.Time, o billingReportOptions) (billingEventsDTO, error) {
	w := billingPeriod(at, o.period)
	out := billingEventsDTO{AsOf: at, Timezone: billingTimezone, Period: o.period, From: w.From, To: w.To, Page: o.page, PageSize: o.size, Items: []billingEventDTO{}}
	query := func() *gorm.DB {
		return db.Table("billing_events").Where("user_id = ? AND published = TRUE AND occurred_at >= ? AND occurred_at < ? AND occurred_at <= ?", userID, w.From, w.To, at)
	}
	if err := query().Count(&out.Total).Error; err != nil {
		return out, platform.WrapDB(err)
	}
	if err := query().Select("id, event_type, occurred_at, instance_id, instance_name, grant_source, amount_pt, available_before_pt, available_after_pt, hourly_before_pt, hourly_after_pt").Order("occurred_at DESC, id DESC").Offset((o.page - 1) * o.size).Limit(o.size).Find(&out.Items).Error; err != nil {
		return out, platform.WrapDB(err)
	}
	for i := range out.Items {
		out.Items[i].OccurredAt = out.Items[i].OccurredAt.In(zone)
	}
	return out, nil
}
