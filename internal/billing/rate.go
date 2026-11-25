package billing

import (
	"fmt"
	"math/big"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/billing/v1alpha1"
)

// Item is one thing billed, as the meter records it and a price list
// prices it.
type Item struct {
	// Name is the Prices field that prices it, such as "cpu".
	Name string
	// Unit is what a price is for an hour of, such as "vCPU-hour".
	Unit string
	// PerUnit is how much of the meter's quantity is one unit: millicores
	// per vCPU and MiB per GiB of memory; 1 for the rest.
	PerUnit int64
}

// Items are the things billed, in the order a Statement lists them.
var Items = []Item{
	{"cpu", "vCPU-hour", 1000},
	{"memory", "GiB-hour", 1024},
	{"disk", "GiB-hour", 1},
	{"addresses", "address-hour", 1},
	{"public_endpoints", "endpoint-hour", 1},
	{"port_forwards", "port-forward-hour", 1},
	{"file_shares", "file-share-hour", 1},
	{"network_endpoints", "endpoint-hour", 1},
	{"domains", "domain-hour", 1},
}

// PriceSet is a set of prices by item name; an item not in it is free.
type PriceSet map[string]Micros

// ParsePrices reads p, refusing a price that is not a decimal of at most six
// digits after the point.
func ParsePrices(p *billingv1alpha1.Prices) (PriceSet, error) {
	fields := map[string]string{
		"cpu": p.GetCpu(), "memory": p.GetMemory(), "disk": p.GetDisk(), "addresses": p.GetAddresses(),
		"public_endpoints": p.GetPublicEndpoints(), "port_forwards": p.GetPortForwards(),
		"file_shares": p.GetFileShares(), "network_endpoints": p.GetNetworkEndpoints(), "domains": p.GetDomains(),
	}
	out := PriceSet{}
	for _, it := range Items {
		s := fields[it.Name]
		if s == "" {
			continue
		}
		m, err := ParseMoney(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", it.Name, err)
		}
		out[it.Name] = m
	}
	return out, nil
}

// Entry is a price list's entry, read.
type Entry struct {
	Effective time.Time
	Prices    PriceSet
}

// ParseEntries reads a price list's entries, refusing one whose time is
// missing or not on a whole minute, or not after the one before.
func ParseEntries(entries []*billingv1alpha1.PriceEntry) ([]Entry, error) {
	out := make([]Entry, 0, len(entries))
	for i, e := range entries {
		if e.GetEffectiveTime() == nil {
			return nil, fmt.Errorf("entries[%d]: effective_time is required", i)
		}
		at := e.GetEffectiveTime().AsTime()
		if !at.Equal(at.Truncate(time.Minute)) {
			return nil, fmt.Errorf("entries[%d]: effective_time %s is not on a whole minute", i, at.Format(time.RFC3339Nano))
		}
		if i > 0 && !at.After(out[i-1].Effective) {
			return nil, fmt.Errorf("entries[%d]: effective_time %s is not after the entry before's", i, at.Format(time.RFC3339))
		}
		prices, err := ParsePrices(e.GetPrices())
		if err != nil {
			return nil, fmt.Errorf("entries[%d]: %w", i, err)
		}
		out = append(out, Entry{Effective: at, Prices: prices})
	}
	return out, nil
}

// Span is a part of a period rated at one entry's prices; Entry is nil for
// one before the first entry, which is free.
type Span struct {
	From, To time.Time
	Entry    *Entry
}

// Spans cuts [from, to) where an entry of entries takes effect.
func Spans(entries []Entry, from, to time.Time) []Span {
	var out []Span
	cur := from
	var in *Entry
	for i := range entries {
		e := &entries[i]
		if !e.Effective.After(cur) {
			in = e
			continue
		}
		if !e.Effective.Before(to) {
			break
		}
		out = append(out, Span{From: cur, To: e.Effective, Entry: in})
		cur, in = e.Effective, e
	}
	if cur.Before(to) {
		out = append(out, Span{From: cur, To: to, Entry: in})
	}
	return out
}

// Line is one thing billed.
type Line struct {
	Item string
	// Quantity is in units, such as vCPU-hours, to a millionth.
	Quantity  Micros
	Unit      string
	UnitPrice Micros
	Amount    Micros
	// Effective is when UnitPrice took effect; zero for a fee.
	Effective time.Time
}

// Proto is l as a Statement line.
func (l Line) Proto() *billingv1alpha1.StatementLine {
	out := &billingv1alpha1.StatementLine{
		Item: l.Item, Quantity: l.Quantity.String(), Unit: l.Unit,
		UnitPrice: l.UnitPrice.String(), Amount: l.Amount.String(),
	}
	if !l.Effective.IsZero() {
		out.PriceEffectiveTime = timestamppb.New(l.Effective)
	}
	return out
}

// Rate is the lines for what was held over span, held as item name to the
// sum of the meter's quantity over its minutes. An item free in the span,
// or not held, has no line.
func Rate(span Span, held map[string]int64) []Line {
	if span.Entry == nil {
		return nil
	}
	var out []Line
	for _, it := range Items {
		q := held[it.Name]
		price, priced := span.Entry.Prices[it.Name]
		if q == 0 || !priced {
			continue
		}
		// A minute is a sixtieth of the hour a price is for.
		perHour := big.NewInt(it.PerUnit * 60)
		amount := divRound(new(big.Int).Mul(big.NewInt(q), big.NewInt(int64(price))), perHour)
		quantity := divRound(new(big.Int).Mul(big.NewInt(q), big.NewInt(perUnit)), perHour)
		out = append(out, Line{
			Item: it.Name, Quantity: Micros(quantity.Int64()), Unit: it.Unit,
			UnitPrice: price, Amount: Micros(amount.Int64()), Effective: span.Entry.Effective,
		})
	}
	return out
}

// FeeLine is the line for a monthly fee over the part of a period that
// passed, passed of whole.
func FeeLine(fee Micros, passed, whole time.Duration) Line {
	if passed >= whole {
		return Line{Item: "monthly_fee", Quantity: perUnit, Unit: "month", UnitPrice: fee, Amount: fee}
	}
	return Line{
		Item: "monthly_fee", Quantity: Micros(perUnit).Share(int64(passed/time.Minute), int64(whole/time.Minute)),
		Unit: "month", UnitPrice: fee, Amount: fee.Share(int64(passed/time.Minute), int64(whole/time.Minute)),
	}
}

// Total is what lines come to, rounded to currency's minor unit.
func Total(lines []Line, currency string) Micros {
	var sum Micros
	for _, l := range lines {
		sum += l.Amount
	}
	return RoundToMinor(sum, currency)
}
