package billing

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

func TestBillingReportCalendarAndAxis(t *testing.T) {
	at := time.Date(2028, 2, 29, 17, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		period, from, to, granularity string
		count                         int
	}{
		{"today", "2028-03-01T00:00:00+08:00", "2028-03-02T00:00:00+08:00", "hour", 24},
		{"week", "2028-02-28T00:00:00+08:00", "2028-03-06T00:00:00+08:00", "day", 7},
		{"month", "2028-03-01T00:00:00+08:00", "2028-04-01T00:00:00+08:00", "day", 31},
		{"year", "2028-01-01T00:00:00+08:00", "2029-01-01T00:00:00+08:00", "month", 12},
	} {
		w := billingPeriod(at, tc.period)
		axis, granularity := billingAxis(w, tc.period)
		if w.From.Format(time.RFC3339) != tc.from || w.To.Format(time.RFC3339) != tc.to || granularity != tc.granularity || len(axis) != tc.count {
			t.Fatalf("%s: %+v %s %d", tc.period, w, granularity, len(axis))
		}
		for i := 1; i < len(axis); i++ {
			if !axis[i-1].To.Equal(axis[i].From) {
				t.Fatal("axis gap")
			}
		}
	}
	leap := billingPeriod(time.Date(2028, 2, 10, 0, 0, 0, 0, zone), "month")
	if axis, _ := billingAxis(leap, "month"); len(axis) != 29 {
		t.Fatal("leap February lost a day")
	}
}

func TestBillingReportRejectsQueryAmbiguity(t *testing.T) {
	for _, tc := range []struct{ kind, query string }{
		{"overview", "instance_id=1"}, {"overview", "user_id=2"}, {"trend", "period=week&period=month"},
		{"trend", "period=year&account_id=1"}, {"trend", "period=all"}, {"trend", "period=month&instance_id=0"},
		{"trend", "period=month&instance_id=-1"}, {"trend", "period=month&instance_id=18446744073709551616"},
		{"instances", "period=today&page=0"}, {"instances", "period=today&page=2147483648"},
		{"instances", "period=today&page_size=101"}, {"instances", "period=today&page_size=2&page_size=2"},
		{"instances", "period=today&page=+1"}, {"instances", "period=today&search=%ZZ"},
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/?"+tc.query, nil)
		if _, err := billingReportParams(c, tc.kind); err == nil {
			t.Fatalf("accepted %s %s", tc.kind, tc.query)
		}
	}
}

func TestBillingTailClippingConservesCumulativeMicroPointsAndComponents(t *testing.T) {
	from := time.Date(2026, 11, 8, 23, 59, 59, 0, zone)
	s := row{"instance_id": 1, "charge_kind": "base_compute", "config_snapshot": `{"vcpus":2,"memory_bytes":"1073741824"}`, "cpu_hourly_pt": "0.001000", "memory_gib_hourly_pt": "0.001000"}
	tail := billingTail{Session: s, From: from, To: from.Add(4 * time.Second), BaseMicros: 1234567, BaseExpected: expected(s, 1234567)}
	whole := billingTailClip(tail, billingWindow{from, tail.To})
	var total, cpu, memory decimal.Decimal
	for i := range 4 {
		v := billingTailClip(tail, billingWindow{from.Add(time.Duration(i) * time.Second), from.Add(time.Duration(i+1) * time.Second)})
		total = total.Add(v.total)
		cpu = cpu.Add(v.cpu)
		memory = memory.Add(v.memory)
		if !v.cpu.Add(v.memory).Equal(v.total) {
			t.Fatal("tail components do not conserve")
		}
	}
	if !total.Equal(whole.total) || !cpu.Equal(whole.cpu) || !memory.Equal(whole.memory) || !total.Equal(expected(s, 5234567).Sub(expected(s, 1234567))) {
		t.Fatalf("tail partitions changed rounding: %+v %+v", whole, total)
	}
	if got := billingTailClip(tail, billingWindow{tail.To, tail.To.Add(time.Hour)}); !got.total.IsZero() {
		t.Fatal("disjoint tail included")
	}
}

func TestBillingTailStorageAndGPUUseFrozenSnapshots(t *testing.T) {
	from := time.Date(2026, 11, 9, 0, 0, 0, 0, zone)
	for _, tc := range []struct{ kind, config, disk, gpu, want string }{
		{"storage", `{"disk_bytes":"2147483648"}`, "1.250000", "0", "2.500000"},
		{"gpu", `{}`, "0", "3.125000", "3.125000"},
	} {
		s := row{"charge_kind": tc.kind, "config_snapshot": tc.config, "disk_gib_hourly_pt": tc.disk, "gpu_hourly_pt": tc.gpu}
		tail := billingTail{Session: s, From: from, To: from.Add(time.Hour)}
		v := billingTailClip(tail, billingWindow{from, tail.To})
		if money(v.total) != tc.want || !v.cpu.IsZero() || !v.memory.IsZero() || !v.disk.Add(v.gpu).Equal(v.total) {
			t.Fatalf("%s snapshot breakdown: %+v", tc.kind, v)
		}
	}
}
