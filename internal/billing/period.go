package billing

import (
	"fmt"
	"time"

	// Periods are cut in a tenant's time zone, which a host without a zone
	// database still has to know.
	_ "time/tzdata"
)

// MaxPeriodMonths is the longest period a pay-as-you-go account may have.
const MaxPeriodMonths = 6

// LoadZone is the IANA time zone name names, UTC for "".
func LoadZone(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("time zone %q: %w", name, err)
	}
	return loc, nil
}

// Periods cuts time into periods of Months whole months, the first Offset
// months after Anchor, in Zone: period n starts Offset+n*Months months
// after Anchor, on Anchor's day of the month and time of day there, or on
// the month's last day if it has fewer days, as January 31 gives February
// 28 and then March 31 again. Months below 1 count as 1.
type Periods struct {
	Anchor time.Time
	Months int
	Offset int
	Zone   *time.Location
}

func (p Periods) months() int { return max(p.Months, 1) }

// Start is when period n starts.
func (p Periods) Start(n int) time.Time {
	a := p.Anchor.In(p.Zone)
	months := int(a.Month()) - 1 + p.Offset + n*p.months()
	year := a.Year() + floorDiv(months, 12)
	month := time.Month(months - floorDiv(months, 12)*12 + 1)
	day := min(a.Day(), daysIn(year, month, p.Zone))
	return time.Date(year, month, day, a.Hour(), a.Minute(), a.Second(), a.Nanosecond(), p.Zone)
}

// At is the number of the period holding t and its bounds, [start, end);
// a time before the first period is in none, and gives ok false.
func (p Periods) At(t time.Time) (n int, start, end time.Time, ok bool) {
	if t.Before(p.Start(0)) {
		return 0, time.Time{}, time.Time{}, false
	}
	a := p.Anchor.In(p.Zone)
	tl := t.In(p.Zone)
	months := (tl.Year()-a.Year())*12 + int(tl.Month()) - int(a.Month()) - p.Offset
	n = max(months, 0) / p.months()
	// Counting months alone can be one period off either way, when the day
	// of the month or the time of day is short of the anchor's.
	for n > 0 && p.Start(n).After(t) {
		n--
	}
	for !p.Start(n + 1).After(t) {
		n++
	}
	return n, p.Start(n), p.Start(n + 1), true
}

func daysIn(year int, month time.Month, loc *time.Location) int {
	return time.Date(year, month+1, 0, 12, 0, 0, 0, loc).Day()
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}
