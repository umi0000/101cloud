package platform

import "time"

// Hardware inventory has a platform lifetime, independent of GPU scheduling.
const NodeInventoryPollInterval = 20 * time.Second
const NodeInventoryMaxAge = 60 * time.Second

// ObservationFresh rejects missing and future observations as well as expired ones.
func ObservationFresh(now, observed time.Time, maxAge time.Duration) bool {
	return maxAge > 0 && !observed.IsZero() && !observed.After(now) && now.Sub(observed) <= maxAge
}

func InventoryFresh(now time.Time, observed any) bool {
	return ObservationFresh(now, Stamp(observed), NodeInventoryMaxAge)
}
