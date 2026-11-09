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

type billingInstanceOptionDTO struct {
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	Deleted    bool   `json:"deleted"`
}

type billingInstanceOptionsDTO struct {
	AsOf     time.Time                  `json:"as_of"`
	Page     int                        `json:"page"`
	PageSize int                        `json:"page_size"`
	Total    int64                      `json:"total"`
	Items    []billingInstanceOptionDTO `json:"items"`
}

func billingInstanceOptionsParams(c *gin.Context) (billingReportOptions, error) {
	o := billingReportOptions{page: 1, size: 20}
	q, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		return o, invalid("invalid query parameters")
	}
	for key, values := range q {
		if !in(key, "search", "page", "page_size") || len(values) != 1 {
			return o, invalid("unknown or duplicate query parameter: " + key)
		}
	}
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
	return o, nil
}

func registerBillingInstanceOptions(a *platform.App, user *gin.RouterGroup) {
	user.GET("/billing/instance-options", platform.Handle(func(c *gin.Context) (any, error) {
		o, err := billingInstanceOptionsParams(c)
		if err != nil {
			return nil, err
		}
		out := billingInstanceOptionsDTO{Page: o.page, PageSize: o.size, Items: []billingInstanceOptionDTO{}}
		err = platform.DB(c, a).Transaction(func(db *gorm.DB) error {
			filter := "user_id = ?"
			args := []any{platform.Current(c).ID}
			if o.search != "" {
				// LOCATE preserves literal percent/underscore search semantics.
				filter += " AND (LOCATE(?,logical_name)>0 OR CAST(id AS CHAR)=?)"
				args = append(args, o.search, o.search)
			}
			if e := db.Table("instances").Where(filter, args...).Count(&out.Total).Error; e != nil {
				return e
			}
			out.AsOf = now()
			var items []struct {
				ID      uint64
				Name    string
				Deleted bool
			}
			if e := db.Raw("SELECT id,logical_name AS name,deleted_at IS NOT NULL AS deleted FROM instances WHERE "+filter+" ORDER BY id DESC LIMIT ? OFFSET ?", append(args, o.size, int64(o.page-1)*int64(o.size))...).Scan(&items).Error; e != nil {
				return e
			}
			for _, item := range items {
				out.Items = append(out.Items, billingInstanceOptionDTO{InstanceID: sid(item.ID), Name: item.Name, Deleted: item.Deleted})
			}
			return nil
		}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		return out, err
	}))
}
