package main

import (
	"context"
	"time"

	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/domain/events"
	"github.com/flexprice/flexprice/internal/types"
)

// writer is the Flexprice side of provisioning. The REST client and the
// in-process service layer both implement it, so the provisioner is the same
// whether it talks to an API host or straight to a database.
type writer interface {
	customerByExternalID(ctx context.Context, externalID string) (*customerRecord, error)
	createCustomer(ctx context.Context, req map[string]any) (*customerRecord, error)
	featuresByLookupKeys(ctx context.Context, keys []string) (map[string]featureRecord, error)
	createFeature(ctx context.Context, req map[string]any) (*featureRecord, error)
	planByLookupKey(ctx context.Context, key string) (*planRecord, error)
	createPlan(ctx context.Context, req map[string]any) (*planRecord, error)
	createPrice(ctx context.Context, req map[string]any) (*priceRecord, error)
	// planPrices lists a plan's prices including those that already ended.
	planPrices(ctx context.Context, planID string) ([]priceRecord, error)
	createEntitlement(ctx context.Context, req map[string]any) error
	// subscriptionsForCustomer lists the customer's active subscriptions.
	subscriptionsForCustomer(ctx context.Context, externalID string) ([]subscriptionRecord, error)
	cancelledSubscriptions(ctx context.Context, customerID string) ([]subscriptionRecord, error)
	createSubscription(ctx context.Context, req map[string]any) (*subscriptionRecord, error)
	updatePlan(ctx context.Context, id string, req map[string]any) error
	// syncPlanPrices adds missing plan prices as line items on the plan's
	// active subscriptions; returns the number created (-1 when asynchronous).
	syncPlanPrices(ctx context.Context, planID string) (int, error)
	updateCustomer(ctx context.Context, id string, req map[string]any) error
	cancelSubscription(ctx context.Context, id string, req map[string]any) error
	// ingestEvents stores a batch of usage events so that meters see them.
	ingestEvents(ctx context.Context, batch []*events.Event) error
	deletePlan(ctx context.Context, id string) error
	deleteCustomer(ctx context.Context, id string) error
	// updateBillingPeriods rolls every active subscription in the environment
	// forward to the current period, invoicing each elapsed one.
	updateBillingPeriods(ctx context.Context) (*dto.SubscriptionUpdatePeriodResponse, error)
	// terminatePrice ends a price at end; a price ended at its own start never
	// produces a line item.
	terminatePrice(ctx context.Context, id string, end time.Time) error
	invoicesForSubscription(ctx context.Context, subscriptionID string) ([]invoiceRecord, error)
	invoice(ctx context.Context, id string) (*invoiceRecord, error)
	creditNotesForInvoice(ctx context.Context, invoiceID string) ([]creditNoteRecord, error)
	createCreditNote(ctx context.Context, req map[string]any) (*creditNoteRecord, error)
	// usageForSubscription prices the subscription's metered usage in the
	// window the way invoice creation does.
	usageForSubscription(ctx context.Context, subscriptionID string, start, end time.Time) (*dto.GetUsageBySubscriptionResponse, error)
	// listInvoices returns every invoice in the environment in the given
	// invoice status, or in any status when it is empty.
	listInvoices(ctx context.Context, status string) ([]invoiceRecord, error)
}

// dbAdmin is the repair surface the REST API does not expose; only the
// database writer implements it.
type dbAdmin interface {
	// archivePrice removes a price nothing references (a duplicate).
	archivePrice(ctx context.Context, id string) error
	invoiceConfig(ctx context.Context) (types.InvoiceConfig, error)
	// invoiceDueDate applies the subscription's payment terms, else the
	// tenant's, to an issue date the way invoice finalization does.
	invoiceDueDate(ctx context.Context, subscriptionID string, issue time.Time) (time.Time, error)
	// redateInvoice rewrites a finalized invoice's number, issue date and due date.
	redateInvoice(ctx context.Context, id, number string, issue, due time.Time) error
	// setInvoiceSequence fixes the last number handed out for a year-month so
	// the next invoice continues after the renumbered ones.
	setInvoiceSequence(ctx context.Context, yearMonth string, last int64) error
	// purgeInvoices hard-deletes the invoices and their line items.
	purgeInvoices(ctx context.Context, ids []string) (int, error)
}
