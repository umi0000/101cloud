package billing

import (
	"net/http/httptest"
	"testing"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

func TestBillingEventsParamsDefaultsAndPagination(t *testing.T) {
	for _, tc := range []struct {
		query, period string
		page, size    int
	}{
		{"period=today", "today", 1, 20},
		{"period=week", "week", 1, 20},
		{"period=month&page=2&page_size=1", "month", 2, 1},
		{"period=year&page=2147483647&page_size=100", "year", 2147483647, 100},
		{"period=today&page=0002&page_size=0020", "today", 2, 20},
	} {
		t.Run(tc.query, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/?"+tc.query, nil)
			got, err := billingEventsParams(c)
			if err != nil {
				t.Fatal(err)
			}
			if got.period != tc.period || got.page != tc.page || got.size != tc.size || got.instance != 0 || got.search != "" {
				t.Fatalf("options=%+v, want period=%s page=%d size=%d", got, tc.period, tc.page, tc.size)
			}
		})
	}
}

func TestBillingEventsParamsRejectMalformedDuplicateAndUnknownFilters(t *testing.T) {
	for _, query := range []string{
		"", "period", "period=", "period=all", "period=Today", "period=%20today", "period=today%20",
		"period=today&period=today", "period=today&period=month", "period=today&%70eriod=week",
		"period=today&page=1&page=1", "period=today&page_size=20&page_size=20",
		"period=today&account_id=1", "period=today&user_id=2", "period=today&instance_id=1",
		"period=today&search=x", "period=today&event_type=grant", "period=today&published=true",
		"period=today&from=2026-01-01", "period=today&to=2027-01-01", "period=today&timezone=UTC",
		"period=today&unknown", "period=today&unknown=1&unknown=2",
		"period=%ZZ", "period=today&page=%ZZ", "period=today;page=2",
		"period=today&page=", "period=today&page=0", "period=today&page=-1", "period=today&page=+1",
		"period=today&page=%2B1", "period=today&page=%201", "period=today&page=1%20",
		"period=today&page=1.0", "period=today&page=1e2", "period=today&page=0x10",
		"period=today&page=%EF%BC%91", "period=today&page=2147483648", "period=today&page=18446744073709551616",
		"period=today&page_size=", "period=today&page_size=0", "period=today&page_size=-1",
		"period=today&page_size=+1", "period=today&page_size=1.0", "period=today&page_size=101",
		"period=today&page_size=2147483648", "period=today&page_size=18446744073709551616",
	} {
		t.Run(query, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/?"+query, nil)
			_, err := billingEventsParams(c)
			he, ok := err.(*platform.HTTPError)
			if !ok || he.Status != 422 {
				t.Fatalf("query=%q error=%#v, want validation HTTP 422", query, err)
			}
		})
	}
}
