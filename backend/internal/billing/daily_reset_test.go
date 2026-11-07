package billing

import (
	"testing"
	"time"
)

func TestDailyExpiryUsesCalendarResetAndEarlierTTL(t *testing.T) {
	for _, tc := range []struct {
		name, timezone, clock, at, expiry string
		ttl                               uint64
	}{
		{"before_reset", "Asia/Shanghai", "06:30:00", "2026-11-07T06:29:59+08:00", "2026-11-07T06:30:00+08:00", 604800},
		{"at_reset", "Asia/Shanghai", "06:30:00", "2026-11-07T06:30:00+08:00", "2026-11-08T06:30:00+08:00", 604800},
		{"short_TTL", "Asia/Shanghai", "06:30:00", "2026-11-07T07:00:00+08:00", "2026-11-07T07:01:00+08:00", 60},
		{"spring_23_hour_cycle", "America/New_York", "06:30:00", "2026-03-07T06:30:00-05:00", "2026-03-08T06:30:00-04:00", 604800},
		{"fall_25_hour_cycle", "America/New_York", "06:30:00", "2026-10-31T06:30:00-04:00", "2026-11-01T06:30:00-05:00", 604800},
		{"fall_TTL_earlier", "America/New_York", "06:30:00", "2026-10-31T06:30:00-04:00", "2026-11-01T05:30:00-05:00", 86400},
		{"spring_gap_does_not_shift_next_clock", "America/New_York", "02:30:00", "2026-03-08T03:00:00-04:00", "2026-03-09T02:30:00-04:00", 604800},
		{"day_after_gap_before_reset", "America/New_York", "02:30:00", "2026-03-09T02:00:00-04:00", "2026-03-09T02:30:00-04:00", 604800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(time.RFC3339, tc.expiry)
			if err != nil {
				t.Fatal(err)
			}
			p := row{"timezone": tc.timezone, "grant_time": tc.clock, "daily_validity_seconds": tc.ttl}
			cycle, err := cycleAt(p, at)
			if err != nil {
				t.Fatal(err)
			}
			got, err := dailyExpiry(p, cycle, at)
			if err != nil || !got.Equal(want) {
				t.Fatalf("expiry=%s want=%s error=%v", got, want, err)
			}
		})
	}
}

func TestDailyExpiryRejectsMalformedStoredPolicy(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for _, p := range []row{
		nil,
		{"timezone": "", "grant_time": "06:30:00", "daily_validity_seconds": 60},
		{"timezone": "Not/A_Zone", "grant_time": "06:30:00", "daily_validity_seconds": 60},
		{"timezone": "UTC", "grant_time": "25:00:00", "daily_validity_seconds": 60},
		{"timezone": "UTC", "grant_time": "06:30:00", "daily_validity_seconds": 0},
		{"timezone": "UTC", "grant_time": "06:30:00", "daily_validity_seconds": ^uint64(0)},
	} {
		if _, err := dailyExpiry(p, at, at); err == nil {
			t.Fatalf("accepted malformed stored policy: %#v", p)
		}
	}
}
