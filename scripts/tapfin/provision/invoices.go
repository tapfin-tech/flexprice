package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/flexprice/flexprice/internal/types"
)

// purgeZeroInvoices hard-deletes every zero-total SKIPPED invoice in the
// environment: the empty drafts left behind by subscription creation and by
// cycles that had no usage.
func (p *provisioner) purgeZeroInvoices(ctx context.Context) error {
	admin, ok := p.api.(dbAdmin)
	if !ok {
		return errors.New("purge-zero-invoices needs -mode db: the REST API cannot delete invoices")
	}
	invs, err := p.api.listInvoices(ctx, string(types.InvoiceStatusSkipped))
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(p.opt.timezone)
	if err != nil {
		return err
	}
	var ids []string
	byReason := map[string]int{}
	for _, inv := range invs {
		if !inv.Total.IsZero() {
			continue
		}
		ids = append(ids, inv.ID)
		byReason[inv.BillingReason]++
		fmt.Printf("%s %-20s customer=%s period %s..%s\n", inv.ID, inv.BillingReason, inv.CustomerID,
			formatDay(inv.PeriodStart, loc), formatDay(inv.PeriodEnd, loc))
	}
	fmt.Printf("\nzero invoices: %d", len(ids))
	for reason, n := range byReason {
		fmt.Printf("  %s=%d", reason, n)
	}
	fmt.Println()
	if len(ids) == 0 {
		return nil
	}
	if !p.write() {
		fmt.Println("[dry-run] -apply deletes them permanently")
		return nil
	}
	n, err := admin.purgeInvoices(ctx, ids)
	if err != nil {
		return err
	}
	fmt.Printf("deleted %d invoices\n", n)
	return nil
}

// purgeCancelledInvoices hard-deletes every invoice, with its credit notes,
// raised for the selected organisations' cancelled subscriptions.
func (p *provisioner) purgeCancelledInvoices(ctx context.Context, orgs []organization) error {
	admin, ok := p.api.(dbAdmin)
	if !ok {
		return errors.New("purge-cancelled-invoices needs -mode db: the REST API cannot delete invoices")
	}
	loc, err := time.LoadLocation(p.opt.timezone)
	if err != nil {
		return err
	}
	var ids []string
	for _, org := range orgs {
		cust, err := p.api.customerByExternalID(ctx, org.Code)
		if err != nil {
			return fmt.Errorf("org %s: lookup customer: %w", org.Code, err)
		}
		if cust == nil {
			continue
		}
		subs, err := p.api.cancelledSubscriptions(ctx, cust.ID)
		if err != nil {
			return fmt.Errorf("org %s: list subscriptions: %w", org.Code, err)
		}
		for _, s := range subs {
			invs, err := p.api.invoicesForSubscription(ctx, s.ID)
			if err != nil {
				return fmt.Errorf("subscription %s: list invoices: %w", s.ID, err)
			}
			for _, inv := range invs {
				notes, err := p.api.creditNotesForInvoice(ctx, inv.ID)
				if err != nil {
					return fmt.Errorf("invoice %s: list credit notes: %w", inv.ID, err)
				}
				fmt.Printf("%-12s %s %-16s %-9s %10s period %s..%s credit notes %d\n", org.Code, inv.ID, inv.InvoiceNumber,
					inv.InvoiceStatus, inv.Total.StringFixed(2), formatDay(inv.PeriodStart, loc), formatDay(inv.PeriodEnd, loc), len(notes))
				ids = append(ids, inv.ID)
			}
		}
	}
	fmt.Printf("\ninvoices on cancelled subscriptions: %d\n", len(ids))
	if len(ids) == 0 {
		return nil
	}
	if !p.write() {
		fmt.Println("[dry-run] -apply deletes them and their credit notes permanently")
		return nil
	}
	n, err := admin.purgeInvoices(ctx, ids)
	if err != nil {
		return err
	}
	fmt.Printf("deleted %d invoices\n", n)
	return nil
}

func formatDay(t *time.Time, loc *time.Location) string {
	if t == nil {
		return "-"
	}
	return t.In(loc).Format("2006-01-02")
}

// invoiceRedate is the identity a finalized invoice gets once it is dated at
// the end of the period it bills.
type invoiceRedate struct {
	inv    invoiceRecord
	number string
	issue  time.Time
	due    time.Time
}

func (r invoiceRedate) changed() bool {
	return r.number != r.inv.InvoiceNumber || r.inv.IssueDate == nil || !r.inv.IssueDate.Equal(r.issue) ||
		r.inv.DueDate == nil || !r.inv.DueDate.Equal(r.due)
}

// sequenceKey is the invoice-number segment the numbering config derives
// from the issue date; one counter runs per key.
func sequenceKey(cfg types.InvoiceConfig, issue time.Time) (string, error) {
	loc, err := time.LoadLocation(types.ResolveTimezone(cfg.InvoiceNumberTimezone))
	if err != nil {
		loc = time.UTC
	}
	t := issue.In(loc)
	switch cfg.InvoiceNumberFormat {
	case types.InvoiceNumberFormatYYYYMM:
		return t.Format("200601"), nil
	case types.InvoiceNumberFormatYYYY:
		return t.Format("2006"), nil
	case types.InvoiceNumberFormatYYMMDD:
		return t.Format("060102"), nil
	case types.InvoiceNumberFormatYYYYMMDD:
		return t.Format("20060102"), nil
	case types.InvoiceNumberFormatYY:
		return t.Format("06"), nil
	}
	return "", fmt.Errorf("unsupported invoice number format %q", cfg.InvoiceNumberFormat)
}

// planRedates dates every finalized subscription invoice at the end of its
// billing period, the moment the monthly cron raises it, and numbers the
// invoices in period order within each sequence key. It returns the plan and
// the last number used per key.
func planRedates(invs []invoiceRecord, cfg types.InvoiceConfig) ([]invoiceRedate, map[string]int64, error) {
	plan := make([]invoiceRedate, 0, len(invs))
	for _, inv := range invs {
		if inv.PeriodEnd == nil || inv.SubscriptionID == "" {
			return nil, nil, fmt.Errorf("invoice %s (%s) is not a subscription period invoice", inv.ID, inv.InvoiceNumber)
		}
		plan = append(plan, invoiceRedate{inv: inv, issue: inv.PeriodEnd.UTC()})
	}
	sort.SliceStable(plan, func(i, j int) bool {
		a, b := plan[i], plan[j]
		if !a.issue.Equal(b.issue) {
			return a.issue.Before(b.issue)
		}
		if a.inv.InvoiceNumber != b.inv.InvoiceNumber {
			return a.inv.InvoiceNumber < b.inv.InvoiceNumber
		}
		return a.inv.ID < b.inv.ID
	})
	last := map[string]int64{}
	for i := range plan {
		key, err := sequenceKey(cfg, plan[i].issue)
		if err != nil {
			return nil, nil, err
		}
		seq, ok := last[key]
		if !ok {
			seq = int64(cfg.InvoiceNumberStartSequence) - 1
		}
		seq++
		last[key] = seq
		plan[i].number = fmt.Sprintf("%s%s%s%s%0*d", cfg.InvoiceNumberPrefix, cfg.InvoiceNumberSeparator, key,
			cfg.InvoiceNumberSeparator, cfg.InvoiceNumberSuffixLength, seq)
	}
	return plan, last, nil
}

// redateInvoices renumbers and redates every finalized invoice in the
// environment as if the billing cron had raised it at period end, then
// moves the number sequences past the last number used.
func (p *provisioner) redateInvoices(ctx context.Context) error {
	admin, ok := p.api.(dbAdmin)
	if !ok {
		return errors.New("redate-invoices needs -mode db: the REST API cannot renumber invoices")
	}
	cfg, err := admin.invoiceConfig(ctx)
	if err != nil {
		return err
	}
	invs, err := p.api.listInvoices(ctx, string(types.InvoiceStatusFinalized))
	if err != nil {
		return err
	}
	plan, last, err := planRedates(invs, cfg)
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(p.opt.timezone)
	if err != nil {
		return err
	}
	changed := 0
	for i := range plan {
		r := &plan[i]
		if r.due, err = admin.invoiceDueDate(ctx, r.inv.SubscriptionID, r.issue); err != nil {
			return fmt.Errorf("invoice %s: %w", r.inv.ID, err)
		}
		mark := " "
		if r.changed() {
			mark = "*"
			changed++
		}
		fmt.Printf("%s %-16s -> %-16s period %s..%s issue %s due %s\n", mark, r.inv.InvoiceNumber, r.number,
			formatDay(r.inv.PeriodStart, loc), formatDay(r.inv.PeriodEnd, loc), formatDay(&r.issue, loc), formatDay(&r.due, loc))
	}
	fmt.Printf("\nfinalized invoices: %d, to change: %d\n", len(plan), changed)
	if changed == 0 {
		return nil
	}
	if !p.write() {
		fmt.Println("[dry-run] -apply rewrites numbers, issue and due dates and resets the number sequences")
		return nil
	}
	// Numbers are unique per environment, so every changed invoice first
	// moves to a placeholder before the final numbers are handed out.
	for _, r := range plan {
		if !r.changed() {
			continue
		}
		if err := admin.redateInvoice(ctx, r.inv.ID, "tmp-"+r.inv.ID, r.issue, r.due); err != nil {
			return fmt.Errorf("invoice %s: %w", r.inv.ID, err)
		}
	}
	for _, r := range plan {
		if !r.changed() {
			continue
		}
		if err := admin.redateInvoice(ctx, r.inv.ID, r.number, r.issue, r.due); err != nil {
			return fmt.Errorf("invoice %s: %w", r.inv.ID, err)
		}
	}
	for key, seq := range last {
		if err := admin.setInvoiceSequence(ctx, key, seq); err != nil {
			return fmt.Errorf("sequence %s: %w", key, err)
		}
		fmt.Printf("sequence %s -> %d\n", key, seq)
	}
	fmt.Printf("redated %d invoices\n", changed)
	return nil
}
