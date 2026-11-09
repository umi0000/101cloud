package billing

import (
	"time"

	"github.com/shopspring/decimal"
)

// Reporting is read-only. All callers share one database snapshot and one clock
// per response; live estimates never mutate usage, reservations, or the ledger.
type billingLive struct {
	Rates    map[uint64]billingRate
	Tails    []billingTail
	Current  billingCurrentDTO
	Forecast billingForecastDTO
	Balance  billingBalanceDTO
	Partial  bool
	Reasons  []string
}

type billingRate struct {
	Hourly decimal.Decimal
	Status string
}

// BaseMicros preserves the billing engine's cumulative micro-point rounding.
// From is the persisted last-settled boundary, not a requested reporting start.
type billingTail struct {
	Session      row
	From         time.Time
	To           time.Time
	BaseMicros   uint64
	BaseExpected decimal.Decimal
}

type billingCurrentDTO struct {
	HourlyPT *string `json:"hourly_pt"`
	Status   string  `json:"status"`
	Reason   *string `json:"reason"`
}

type billingForecastDTO struct {
	Status     string  `json:"status"`
	Seconds    *string `json:"seconds"`
	ExhaustsAt *string `json:"exhausts_at"`
	Reason     *string `json:"reason"`
	Basis      string  `json:"basis"`
}

type billingBalanceDTO struct {
	AvailablePT       string `json:"available_pt"`
	ReservedPT        string `json:"reserved_pt"`
	OutstandingDebtPT string `json:"outstanding_debt_pt"`
}
