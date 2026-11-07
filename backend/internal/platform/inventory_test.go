package platform

import (
	"testing"
	"time"
)

func TestInventoryFreshnessBoundary(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		observed time.Time
		fresh    bool
	}{
		{"missing", time.Time{}, false},
		{"future", now.Add(time.Microsecond), false},
		{"new", now, true},
		{"sixty_seconds", now.Add(-60 * time.Second), true},
		{"expired", now.Add(-60*time.Second - time.Microsecond), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InventoryFresh(now, tc.observed); got != tc.fresh {
				t.Fatalf("fresh=%v want %v", got, tc.fresh)
			}
		})
	}
}
