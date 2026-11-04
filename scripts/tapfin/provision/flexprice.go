package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/domain/events"
	"github.com/shopspring/decimal"
)

// flexprice is a thin REST client for the Flexprice private API.
type flexprice struct {
	baseURL string
	apiKey  string
	envID   string
	http    *http.Client
}

func newFlexprice() (*flexprice, error) {
	base := strings.TrimRight(os.Getenv("FLEXPRICE_API_URL"), "/")
	key := os.Getenv("FLEXPRICE_API_KEY")
	if base == "" || key == "" {
		return nil, fmt.Errorf("FLEXPRICE_API_URL and FLEXPRICE_API_KEY must be set")
	}
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return &flexprice{
		baseURL: base,
		apiKey:  key,
		envID:   os.Getenv("FLEXPRICE_ENVIRONMENT_ID"),
		http:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

type apiError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
	Body    string
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("flexprice %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("flexprice %d: %s", e.Status, e.Body)
}

func (e *apiError) notFound() bool { return e.Status == http.StatusNotFound }

func (f *flexprice) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", f.apiKey)
	req.Header.Set("Content-Type", "application/json")
	if f.envID != "" {
		req.Header.Set("X-Environment-ID", f.envID)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		apiErr := &apiError{Status: resp.StatusCode, Body: string(raw)}
		_ = json.Unmarshal(raw, apiErr)
		return apiErr
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

type listResponse[T any] struct {
	Items []T `json:"items"`
}

type entity struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	LookupKey string `json:"lookup_key"`
	Status    string `json:"status"`
}

type customerRecord struct {
	entity
	ExternalID string `json:"external_id"`
	Timezone   string `json:"timezone"`
	Email      string `json:"email"`
}

type featureRecord struct {
	entity
	MeterID string `json:"meter_id"`
	Type    string `json:"type"`
}

type priceRecord struct {
	entity
	MeterID   string     `json:"meter_id"`
	Amount    string     `json:"amount"`
	StartDate *time.Time `json:"start_date"`
	EndDate   *time.Time `json:"end_date"`
}

type planRecord struct {
	entity
	Prices       []priceRecord `json:"prices"`
	Entitlements []struct {
		ID        string `json:"id"`
		FeatureID string `json:"feature_id"`
	} `json:"entitlements"`
}

type subscriptionRecord struct {
	ID                 string    `json:"id"`
	PlanID             string    `json:"plan_id"`
	SubscriptionStatus string    `json:"subscription_status"`
	Timezone           string    `json:"timezone"`
	CurrentPeriodStart time.Time `json:"current_period_start"`
	CurrentPeriodEnd   time.Time `json:"current_period_end"`
	StartDate          time.Time `json:"start_date"`
}

type invoiceLineRecord struct {
	ID          string          `json:"id"`
	DisplayName string          `json:"display_name"`
	Amount      decimal.Decimal `json:"amount"`
	Quantity    decimal.Decimal `json:"quantity"`
}

type invoiceRecord struct {
	ID             string              `json:"id"`
	InvoiceNumber  string              `json:"invoice_number"`
	InvoiceStatus  string              `json:"invoice_status"`
	PaymentStatus  string              `json:"payment_status"`
	BillingReason  string              `json:"billing_reason"`
	CustomerID     string              `json:"customer_id"`
	SubscriptionID string              `json:"subscription_id"`
	Total          decimal.Decimal     `json:"total"`
	AmountDue      decimal.Decimal     `json:"amount_due"`
	PeriodStart    *time.Time          `json:"period_start"`
	PeriodEnd      *time.Time          `json:"period_end"`
	IssueDate      *time.Time          `json:"issue_date"`
	DueDate        *time.Time          `json:"due_date"`
	LineItems      []invoiceLineRecord `json:"line_items"`
}

type creditNoteRecord struct {
	ID               string          `json:"id"`
	CreditNoteNumber string          `json:"credit_note_number"`
	CreditNoteStatus string          `json:"credit_note_status"`
	TotalAmount      decimal.Decimal `json:"total_amount"`
}

func (f *flexprice) customerByExternalID(ctx context.Context, externalID string) (*customerRecord, error) {
	var c customerRecord
	err := f.do(ctx, http.MethodGet, "/customers/external/"+url.PathEscape(externalID), nil, &c)
	if apiErr, ok := err.(*apiError); ok && apiErr.notFound() {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (f *flexprice) createCustomer(ctx context.Context, req map[string]any) (*customerRecord, error) {
	var c customerRecord
	return &c, f.do(ctx, http.MethodPost, "/customers", req, &c)
}

func (f *flexprice) featuresByLookupKeys(ctx context.Context, keys []string) (map[string]featureRecord, error) {
	var resp listResponse[featureRecord]
	req := map[string]any{"lookup_keys": keys, "limit": 1000}
	if err := f.do(ctx, http.MethodPost, "/features/search", req, &resp); err != nil {
		return nil, err
	}
	out := map[string]featureRecord{}
	for _, item := range resp.Items {
		out[item.LookupKey] = item
	}
	return out, nil
}

func (f *flexprice) createFeature(ctx context.Context, req map[string]any) (*featureRecord, error) {
	var fr featureRecord
	return &fr, f.do(ctx, http.MethodPost, "/features", req, &fr)
}

func (f *flexprice) planByLookupKey(ctx context.Context, key string) (*planRecord, error) {
	var resp listResponse[planRecord]
	req := map[string]any{"lookup_key": key, "limit": 10, "expand": "prices,entitlements"}
	if err := f.do(ctx, http.MethodPost, "/plans/search", req, &resp); err != nil {
		return nil, err
	}
	for i := range resp.Items {
		if resp.Items[i].LookupKey == key {
			return &resp.Items[i], nil
		}
	}
	return nil, nil
}

func (f *flexprice) createPlan(ctx context.Context, req map[string]any) (*planRecord, error) {
	var p planRecord
	return &p, f.do(ctx, http.MethodPost, "/plans", req, &p)
}

func (f *flexprice) createPrice(ctx context.Context, req map[string]any) (*priceRecord, error) {
	var p priceRecord
	return &p, f.do(ctx, http.MethodPost, "/prices", req, &p)
}

func (f *flexprice) createEntitlement(ctx context.Context, req map[string]any) error {
	return f.do(ctx, http.MethodPost, "/entitlements", req, nil)
}

func (f *flexprice) subscriptionsForCustomer(ctx context.Context, externalID string) ([]subscriptionRecord, error) {
	var resp listResponse[subscriptionRecord]
	err := f.do(ctx, http.MethodGet, "/customers/external/"+url.PathEscape(externalID)+"/subscriptions", nil, &resp)
	if apiErr, ok := err.(*apiError); ok && apiErr.notFound() {
		return nil, nil
	}
	return resp.Items, err
}

func (f *flexprice) planPrices(ctx context.Context, planID string) ([]priceRecord, error) {
	var resp listResponse[priceRecord]
	err := f.do(ctx, http.MethodGet, "/prices?limit=1000&allow_expired_prices=true&entity_type=PLAN&entity_ids="+url.QueryEscape(planID), nil, &resp)
	return resp.Items, err
}

func (f *flexprice) cancelledSubscriptions(ctx context.Context, customerID string) ([]subscriptionRecord, error) {
	var resp listResponse[subscriptionRecord]
	err := f.do(ctx, http.MethodGet, "/subscriptions?limit=1000&subscription_status=cancelled&customer_id="+url.QueryEscape(customerID), nil, &resp)
	return resp.Items, err
}

func (f *flexprice) createSubscription(ctx context.Context, req map[string]any) (*subscriptionRecord, error) {
	var s subscriptionRecord
	return &s, f.do(ctx, http.MethodPost, "/subscriptions", req, &s)
}

func (f *flexprice) deletePlan(ctx context.Context, id string) error {
	return f.do(ctx, http.MethodDelete, "/plans/"+url.PathEscape(id), nil, nil)
}

func (f *flexprice) deleteCustomer(ctx context.Context, id string) error {
	return f.do(ctx, http.MethodDelete, "/customers/"+url.PathEscape(id), nil, nil)
}

func (f *flexprice) updatePlan(ctx context.Context, id string, req map[string]any) error {
	return f.do(ctx, http.MethodPut, "/plans/"+url.PathEscape(id), req, nil)
}

func (f *flexprice) updateCustomer(ctx context.Context, id string, req map[string]any) error {
	return f.do(ctx, http.MethodPut, "/customers/"+url.PathEscape(id), req, nil)
}

func (f *flexprice) cancelSubscription(ctx context.Context, id string, req map[string]any) error {
	return f.do(ctx, http.MethodPost, "/subscriptions/"+url.PathEscape(id)+"/cancel", req, nil)
}

func (f *flexprice) syncPlanPrices(ctx context.Context, planID string) (int, error) {
	return -1, f.do(ctx, http.MethodPost, "/plans/"+url.PathEscape(planID)+"/sync/subscriptions/v2", nil, nil)
}

func (f *flexprice) ingestEvents(ctx context.Context, batch []*events.Event) error {
	const chunk = 1000
	for start := 0; start < len(batch); start += chunk {
		end := start + chunk
		if end > len(batch) {
			end = len(batch)
		}
		items := make([]map[string]any, 0, end-start)
		for _, e := range batch[start:end] {
			items = append(items, map[string]any{
				"event_id":             e.ID,
				"event_name":           e.EventName,
				"external_customer_id": e.ExternalCustomerID,
				"timestamp":            e.Timestamp.Format(time.RFC3339),
				"source":               e.Source,
				"properties":           e.Properties,
			})
		}
		if err := f.do(ctx, http.MethodPost, "/events/bulk", map[string]any{"events": items}, nil); err != nil {
			return err
		}
	}
	return nil
}

// The period-update cron has no public route; it runs as a Temporal schedule.
func (f *flexprice) updateBillingPeriods(context.Context) (*dto.SubscriptionUpdatePeriodResponse, error) {
	return nil, fmt.Errorf("update-periods needs -mode db: the period-update cron is not exposed on the REST API")
}

func (f *flexprice) usageForSubscription(ctx context.Context, subscriptionID string, start, end time.Time) (*dto.GetUsageBySubscriptionResponse, error) {
	var out dto.GetUsageBySubscriptionResponse
	req := map[string]any{"subscription_id": subscriptionID, "start_time": start.UTC().Format(time.RFC3339), "end_time": end.UTC().Format(time.RFC3339)}
	return &out, f.do(ctx, http.MethodPost, "/subscriptions/usage", req, &out)
}

func (f *flexprice) terminatePrice(ctx context.Context, id string, end time.Time) error {
	req := map[string]any{"end_date": end.UTC().Format(time.RFC3339)}
	return f.do(ctx, http.MethodDelete, "/prices/"+url.PathEscape(id), req, nil)
}

func (f *flexprice) invoicesForSubscription(ctx context.Context, subscriptionID string) ([]invoiceRecord, error) {
	var resp listResponse[invoiceRecord]
	err := f.do(ctx, http.MethodGet, "/invoices?limit=1000&subscription_id="+url.QueryEscape(subscriptionID), nil, &resp)
	return resp.Items, err
}

func (f *flexprice) listInvoices(ctx context.Context, status string) ([]invoiceRecord, error) {
	var resp listResponse[invoiceRecord]
	path := "/invoices?limit=1000"
	if status != "" {
		path += "&invoice_status=" + url.QueryEscape(status)
	}
	err := f.do(ctx, http.MethodGet, path, nil, &resp)
	return resp.Items, err
}

func (f *flexprice) invoice(ctx context.Context, id string) (*invoiceRecord, error) {
	var inv invoiceRecord
	return &inv, f.do(ctx, http.MethodGet, "/invoices/"+url.PathEscape(id), nil, &inv)
}

func (f *flexprice) creditNotesForInvoice(ctx context.Context, invoiceID string) ([]creditNoteRecord, error) {
	var resp listResponse[creditNoteRecord]
	err := f.do(ctx, http.MethodGet, "/creditnotes?limit=100&invoice_id="+url.QueryEscape(invoiceID), nil, &resp)
	return resp.Items, err
}

func (f *flexprice) createCreditNote(ctx context.Context, req map[string]any) (*creditNoteRecord, error) {
	var n creditNoteRecord
	return &n, f.do(ctx, http.MethodPost, "/creditnotes", req, &n)
}
