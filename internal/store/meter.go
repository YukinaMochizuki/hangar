package store

import (
	"context"
	"time"
)

// MeterReading is what one object of a tenant held of one item in a
// minute, in the item's own unit, which the store gives no meaning.
type MeterReading struct {
	UID      string
	Item     string
	Quantity int64
	// Kind, Project and Name say what the object was, for a reader.
	Kind    string
	Project string
	Name    string
}

// MeterTotal is what one object held of one item over a span: Quantity is
// the sum of its minutes' quantities.
type MeterTotal struct {
	UID      string
	Item     string
	Quantity int64
	Kind     string
	Project  string
	Name     string
}

// Meter keeps what billed tenants held, minute by minute
// (docs/decisions/billing.md). It is separate from Store's objects: nothing
// here bumps the revision or reaches a watch. storetest holds the contract.
type Meter interface {
	// RecordMeter makes readings what tenant held in the minute that
	// starts at minute, replacing all it held then, in one transaction.
	// minute is truncated to a whole minute.
	RecordMeter(ctx context.Context, tenant string, minute time.Time, readings []MeterReading) error
	// SumMeter is what tenant held from from to to, each truncated to a
	// whole minute, per object and item, ordered by uid and item. A folded
	// hour counts whole in the span holding its first minute.
	SumMeter(ctx context.Context, tenant string, from, to time.Time) ([]MeterTotal, error)
	// FoldMeter folds tenant's minutes in the whole hours from from to to
	// into one row an hour; the minutes of an hour only partly inside stay
	// minutes. SumMeter answers the same as before for any span that holds
	// each folded hour whole or not at all, as from to to itself does.
	FoldMeter(ctx context.Context, tenant string, from, to time.Time) error
}
