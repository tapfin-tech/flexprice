package main

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// stubFraction is the day-based share of the calendar month that a
// subscription's first period covers: 1 when it starts on the first.
func stubFraction(start time.Time, loc *time.Location) (fraction decimal.Decimal, used, full int) {
	s := start.In(loc)
	monthStart := time.Date(s.Year(), s.Month(), 1, 0, 0, 0, 0, loc)
	monthEnd := monthStart.AddDate(0, 1, 0)
	dayStart := time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, loc)
	full = int(monthEnd.Sub(monthStart).Hours() / 24)
	used = int(monthEnd.Sub(dayStart).Hours() / 24)
	if used >= full {
		return decimal.NewFromInt(1), used, full
	}
	return decimal.NewFromInt(int64(used)).Div(decimal.NewFromInt(int64(full))), used, full
}

// prorateFirstPeriods credits back the unserved share of each selected org's
// first-period invoice. Usage charges are per asset per month and Flexprice
// does not prorate them, so a subscription starting mid-month is billed a
// full month for its stub period; the adjustment credit note reduces the
// amount due to the day-based fraction.
func (p *provisioner) prorateFirstPeriods(ctx context.Context, orgs []organization) error {
	loc, err := time.LoadLocation(p.opt.timezone)
	if err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	for _, org := range orgs {
		subs, err := p.api.subscriptionsForCustomer(ctx, org.Code)
		if err != nil {
			return fmt.Errorf("org %s: list subscriptions: %w", org.Code, err)
		}
		for _, s := range subs {
			if s.SubscriptionStatus != "active" {
				continue
			}
			if err := p.prorateFirstPeriod(ctx, org, s, loc); err != nil {
				return fmt.Errorf("org %s: %w", org.Code, err)
			}
		}
	}
	return nil
}

func (p *provisioner) prorateFirstPeriod(ctx context.Context, org organization, s subscriptionRecord, loc *time.Location) error {
	fraction, used, full := stubFraction(s.StartDate, loc)
	if fraction.Equal(decimal.NewFromInt(1)) {
		p.logf("%s subscription %s starts on the first of the month; nothing to prorate", org.Code, s.ID)
		return nil
	}
	invoices, err := p.api.invoicesForSubscription(ctx, s.ID)
	if err != nil {
		return fmt.Errorf("list invoices for %s: %w", s.ID, err)
	}
	var first *invoiceRecord
	for i := range invoices {
		inv := &invoices[i]
		if inv.InvoiceStatus == "FINALIZED" && inv.PeriodStart != nil && inv.PeriodStart.Equal(s.StartDate) {
			first = inv
			break
		}
	}
	if first == nil {
		p.logf("%s subscription %s: no finalized invoice for the first period yet (run -update-periods first)", org.Code, s.ID)
		return nil
	}
	notes, err := p.api.creditNotesForInvoice(ctx, first.ID)
	if err != nil {
		return fmt.Errorf("list credit notes for %s: %w", first.ID, err)
	}
	for _, n := range notes {
		if n.CreditNoteStatus != "VOIDED" {
			p.logf("%s invoice %s already has credit note %s (%s); skipping", org.Code, first.InvoiceNumber, n.CreditNoteNumber, n.TotalAmount)
			return nil
		}
	}
	inv, err := p.api.invoice(ctx, first.ID)
	if err != nil {
		return fmt.Errorf("get invoice %s: %w", first.ID, err)
	}
	keep := fraction
	credit := decimal.NewFromInt(1).Sub(fraction)
	lines := make([]map[string]any, 0, len(inv.LineItems))
	total := decimal.Zero
	for _, li := range inv.LineItems {
		amount := li.Amount.Mul(credit).Round(2)
		if !amount.IsPositive() {
			continue
		}
		total = total.Add(amount)
		lines = append(lines, map[string]any{
			"invoice_line_item_id": li.ID,
			"display_name":         fmt.Sprintf("%s: prorated to %d of %d days", li.DisplayName, used, full),
			"amount":               amount.String(),
		})
	}
	if len(lines) == 0 {
		p.logf("%s invoice %s has nothing to credit", org.Code, first.InvoiceNumber)
		return nil
	}
	p.logf("%s invoice %s (%s..%s, total %s): keep %d/%d days (%s), credit %s %s",
		org.Code, first.InvoiceNumber, first.PeriodStart.In(loc).Format("2006-01-02"), first.PeriodEnd.In(loc).Format("2006-01-02"),
		inv.Total, used, full, keep.Round(4), total, p.book.Currency)
	p.stats.creditNotes++
	if !p.write() {
		return nil
	}
	note, err := p.api.createCreditNote(ctx, map[string]any{
		"invoice_id":          first.ID,
		"reason":              "ORDER_CHANGE",
		"memo":                fmt.Sprintf("First period prorated: %d of %d days served (subscription started %s)", used, full, s.StartDate.In(loc).Format("2006-01-02")),
		"idempotency_key":     "prorate_first_period_" + first.ID,
		"process_credit_note": true,
		"metadata":            map[string]string{"evision_org_code": org.Code, "proration": "first_period"},
		"line_items":          lines,
	})
	if err != nil {
		return fmt.Errorf("create credit note for %s: %w", first.InvoiceNumber, err)
	}
	p.logf("  credit note %s %s", note.CreditNoteNumber, note.CreditNoteStatus)
	return nil
}
