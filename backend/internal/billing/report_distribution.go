package billing

import (
	"database/sql"
	"net/url"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type billingDistributionInstance struct {
	InstanceID    string `json:"instance_id"`
	Name          string `json:"name"`
	Deleted       bool   `json:"deleted"`
	InstanceState string `json:"instance_state"`
	ConsumedPT    string `json:"consumed_pt"`
	RecordedPT    string `json:"recorded_pt"`
	EstimatedPT   string `json:"estimated_pt"`
}

type billingDistributionDTO struct {
	AsOf        time.Time                     `json:"as_of"`
	Timezone    string                        `json:"timezone"`
	Period      string                        `json:"period"`
	From        time.Time                     `json:"from"`
	To          time.Time                     `json:"to"`
	ConsumedPT  string                        `json:"consumed_pt"`
	RecordedPT  string                        `json:"recorded_pt"`
	EstimatedPT string                        `json:"estimated_pt"`
	Breakdown   billingBreakdown              `json:"breakdown"`
	Instances   []billingDistributionInstance `json:"instances"`
	Partial     bool                          `json:"partial"`
	Reasons     []string                      `json:"reasons"`
}

func billingDistributionParams(c *gin.Context) (string, error) {
	q, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		return "", invalid("invalid query parameters")
	}
	for key, values := range q {
		if key != "period" || len(values) != 1 {
			return "", invalid("unknown or duplicate query parameter: " + key)
		}
	}
	period := q.Get("period")
	if !in(period, "today", "week", "month", "year") {
		return "", invalid("period must be today, week, month or year")
	}
	return period, nil
}

func registerBillingDistribution(a *platform.App, user *gin.RouterGroup) {
	user.GET("/billing/distribution", platform.Handle(func(c *gin.Context) (any, error) {
		period, err := billingDistributionParams(c)
		if err != nil {
			return nil, err
		}
		var result billingDistributionDTO
		err = platform.DB(c, a).Transaction(func(db *gorm.DB) error {
			ac, e := find(db, "credit_accounts", "user_id = ?", platform.Current(c).ID)
			if he, ok := e.(*platform.HTTPError); ok && he.Status == 404 {
				return conflict("policy_not_configured", "an explicit credit allowance account is required")
			}
			if e != nil {
				return e
			}
			// Establish the consistent account read before capturing the report clock.
			at := now()
			live, e := loadBillingUsage(db, ac, at, false)
			if e != nil {
				return e
			}
			result, e = billingDistribution(db, ac, at, live, period)
			return e
		}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		return result, err
	}))
}

func billingDistribution(db *gorm.DB, ac row, at time.Time, live billingLive, period string) (billingDistributionDTO, error) {
	w := billingPeriod(at, period)
	reasons := live.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	out := billingDistributionDTO{AsOf: at, Timezone: billingTimezone, Period: period, From: w.From, To: at, Instances: []billingDistributionInstance{}, Partial: live.Partial, Reasons: reasons}
	query, args := billingAggregateSQL(ac, []billingWindow{{w.From, at}}, live.Tails, 0, true)
	// One unpaged cohort query includes current free workloads and historical
	// positive usage, retaining tombstones and duplicate names by identity.
	query = "WITH report_totals AS (" + query + ") SELECT i.id AS instance_id,i.logical_name AS name,i.deleted_at IS NOT NULL AS deleted,i.observed_state AS instance_state,COALESCE(t.recorded,0) AS recorded,COALESCE(t.estimated,0) AS estimated,COALESCE(t.cpu,0) AS cpu,COALESCE(t.memory,0) AS memory,COALESCE(t.disk,0) AS disk,COALESCE(t.gpu,0) AS gpu FROM instances i LEFT JOIN report_totals t ON t.instance_id=i.id WHERE i.user_id=? AND i.creation_failed_at IS NULL AND ((i.observed_state='running' AND i.deleted_at IS NULL) OR COALESCE(t.recorded,0)+COALESCE(t.estimated,0)>0) ORDER BY COALESCE(t.recorded,0)+COALESCE(t.estimated,0) DESC,i.id DESC"
	args = append(args, ac["user_id"])
	var rows []struct {
		Aggregate     billingAggregate `gorm:"embedded"`
		Name          string
		Deleted       bool
		InstanceState string
	}
	if err := db.Raw(query, args...).Scan(&rows).Error; err != nil {
		return out, err
	}
	recorded, estimated := dec("0"), dec("0")
	var sum billingTailAmounts
	for _, v := range rows {
		a := v.Aggregate
		out.Instances = append(out.Instances, billingDistributionInstance{InstanceID: sid(a.InstanceID), Name: v.Name, Deleted: v.Deleted, InstanceState: v.InstanceState, ConsumedPT: money(a.consumed()), RecordedPT: money(dec(a.Recorded)), EstimatedPT: money(dec(a.Estimated))})
		recorded = recorded.Add(dec(a.Recorded))
		estimated = estimated.Add(dec(a.Estimated))
		sum.cpu = sum.cpu.Add(dec(a.CPU))
		sum.memory = sum.memory.Add(dec(a.Memory))
		sum.disk = sum.disk.Add(dec(a.Disk))
		sum.gpu = sum.gpu.Add(dec(a.GPU))
	}
	out.RecordedPT, out.EstimatedPT, out.ConsumedPT = money(recorded), money(estimated), money(recorded.Add(estimated))
	out.Breakdown = billingBreakdown{money(sum.cpu), money(sum.memory), money(sum.disk), money(sum.gpu)}
	return out, nil
}
