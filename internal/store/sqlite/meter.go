package sqlite

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite/db"
)

var _ store.Meter = (*Store)(nil)

// The meter is written on the write connection like credentials, outside
// s.mu: it takes no revision and no event.

func (s *Store) RecordMeter(ctx context.Context, tenant string, minute time.Time, readings []store.MeterReading) error {
	m := unixMinute(minute)
	return s.credTx(ctx, func(q *db.Queries) error {
		if err := q.ForgetMeterMinutes(ctx, db.ForgetMeterMinutesParams{Tenant: tenant, Minute: m}); err != nil {
			return err
		}
		for _, r := range readings {
			if err := q.RecordMeterMinute(ctx, db.RecordMeterMinuteParams{
				Tenant: tenant, Uid: r.UID, Item: r.Item, Minute: m, Quantity: r.Quantity,
				Kind: r.Kind, Project: r.Project, Name: r.Name,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) SumMeter(ctx context.Context, tenant string, from, to time.Time) ([]store.MeterTotal, error) {
	if s.closing.Load() {
		return nil, store.ErrClosed
	}
	f, t := unixMinute(from), unixMinute(to)
	minutes, err := s.q.SumMeterMinutes(ctx, db.SumMeterMinutesParams{Tenant: tenant, FromMinute: f, ToMinute: t})
	if err != nil {
		return nil, s.closedOr(err)
	}
	hours, err := s.q.SumMeterHours(ctx, db.SumMeterHoursParams{Tenant: tenant, FromMinute: f, ToMinute: t})
	if err != nil {
		return nil, s.closedOr(err)
	}
	type key struct{ uid, item string }
	byKey := map[key]int{}
	var out []store.MeterTotal
	add := func(uid, item string, kind, project, name any, quantity int64) {
		k := key{uid, item}
		if i, ok := byKey[k]; ok {
			out[i].Quantity += quantity
			return
		}
		byKey[k] = len(out)
		out = append(out, store.MeterTotal{UID: uid, Item: item, Quantity: quantity,
			Kind: text(kind), Project: text(project), Name: text(name)})
	}
	for _, r := range minutes {
		add(r.Uid, r.Item, r.Kind, r.Project, r.Name, r.Quantity)
	}
	for _, r := range hours {
		add(r.Uid, r.Item, r.Kind, r.Project, r.Name, r.Quantity)
	}
	slices.SortFunc(out, func(a, b store.MeterTotal) int {
		return cmp.Or(cmp.Compare(a.UID, b.UID), cmp.Compare(a.Item, b.Item))
	})
	return out, nil
}

func (s *Store) FoldMeter(ctx context.Context, tenant string, from, to time.Time) error {
	// The whole hours inside: from rounded up, to rounded down, to an hour.
	f := (unixMinute(from) + 59) / 60 * 60
	t := unixMinute(to) / 60 * 60
	if t <= f {
		return nil
	}
	return s.credTx(ctx, func(q *db.Queries) error {
		span := db.FoldMeterMinutesParams{Tenant: tenant, FromMinute: f, ToMinute: t}
		if err := q.FoldMeterMinutes(ctx, span); err != nil {
			return err
		}
		return q.DeleteMeterMinutesIn(ctx, db.DeleteMeterMinutesInParams{Tenant: tenant, FromMinute: f, ToMinute: t})
	})
}

// unixMinute is t in whole Unix minutes, rounded down.
func unixMinute(t time.Time) int64 {
	return t.Truncate(time.Minute).Unix() / 60
}

// text is a column sqlc types as any, which an aggregate of text is.
func text(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}
