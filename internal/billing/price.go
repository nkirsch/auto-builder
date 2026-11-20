// Package billing bills apps' AI and cloud cost back to their billing
// accounts (TENANCY.md "Billing"): pricing, the Cloud Billing export, Stripe.
package billing

import (
	"math"
	"time"
)

// Pacific is the zone months are counted in, as Google's invoices are.
var Pacific = mustZone("America/Los_Angeles")

func mustZone(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// Month is the first instant of t's Pacific month.
func Month(t time.Time) time.Time {
	t = t.In(Pacific)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, Pacific)
}

// FinalDay is the day of the month from which the previous month is
// final: the billing export lags up to a day.
const FinalDay = 3

// Costs is one app's raw cost in a month.
type Costs struct {
	AI, Runner, Cloud float64
	RunSeconds        int64
}

// Terms price costs: a markup on each and a flat monthly fee.
type Terms struct{ MarkupPct, BaseFee float64 }

// Priced is a statement's amounts in cents.
type Priced struct{ AI, Runner, Cloud, Fee, Total int64 }

// Price marks up each cost (negative costs, from credits, count as zero)
// and adds the fee; Total is the sum of the rounded parts, so invoice lines
// add up to it.
func Price(c Costs, t Terms) Priced {
	up := func(usd float64) int64 {
		return int64(math.Round(max(usd, 0) * (1 + t.MarkupPct/100) * 100))
	}
	p := Priced{AI: up(c.AI), Runner: up(c.Runner), Cloud: up(c.Cloud), Fee: int64(math.Round(t.BaseFee * 100))}
	p.Total = p.AI + p.Runner + p.Cloud + p.Fee
	return p
}

// SplitRunner divides the runner job's cost by each app's run seconds.
func SplitRunner(total float64, seconds map[string]int64) map[string]float64 {
	var all int64
	for _, s := range seconds {
		all += s
	}
	out := map[string]float64{}
	if all == 0 {
		return out
	}
	for slug, s := range seconds {
		out[slug] = total * float64(s) / float64(all)
	}
	return out
}

// MinInvoiceCents is Stripe's smallest charge; less waits for the next invoice.
const MinInvoiceCents = 50
