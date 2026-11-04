package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/cache"
	"github.com/flexprice/flexprice/internal/clickhouse"
	"github.com/flexprice/flexprice/internal/config"
	"github.com/flexprice/flexprice/internal/domain/events"
	"github.com/flexprice/flexprice/internal/domain/invoice"
	"github.com/flexprice/flexprice/internal/domain/meter"
	"github.com/flexprice/flexprice/internal/domain/price"
	"github.com/flexprice/flexprice/internal/domain/subscription"
	"github.com/flexprice/flexprice/internal/ee/service"
	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/integration"
	"github.com/flexprice/flexprice/internal/logger"
	"github.com/flexprice/flexprice/internal/postgres"
	"github.com/flexprice/flexprice/internal/pubsub/memory"
	"github.com/flexprice/flexprice/internal/repository"
	"github.com/flexprice/flexprice/internal/security"
	"github.com/flexprice/flexprice/internal/tracing"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/flexprice/flexprice/internal/utils"
	"github.com/flexprice/flexprice/internal/webhook/publisher"

	"github.com/flexprice/flexprice/ent"
	"github.com/flexprice/flexprice/ent/creditnote"
	"github.com/flexprice/flexprice/ent/creditnotelineitem"
	entinvoice "github.com/flexprice/flexprice/ent/invoice"
	"github.com/flexprice/flexprice/ent/invoicelineitem"
	"github.com/flexprice/flexprice/ent/subscriptionlineitem"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
)

// dbWriter drives the real service layer against the database named by the
// FLEXPRICE_* configuration, scoped to one tenant, environment and user.
type dbWriter struct {
	eventRepo      events.Repository
	meterUsageRepo events.MeterUsageRepository
	meterRepo      meter.Repository
	customers      service.CustomerService
	features       service.FeatureService
	plans          service.PlanService
	prices         service.PriceService
	entitlements   service.EntitlementService
	subscriptions  service.SubscriptionService
	invoices       service.InvoiceService
	creditNotes    service.CreditNoteService
	settings       service.SettingsService
	invoiceRepo    invoice.Repository
	priceRepo      price.Repository
	subRepo        subscription.Repository
	client         postgres.IClient
}

func newDBWriter() (*dbWriter, error) {
	cfg, err := config.NewConfig()
	if err != nil {
		return nil, err
	}
	log, err := logger.NewLogger(cfg)
	if err != nil {
		return nil, err
	}
	tracer := tracing.NewService(cfg, log)
	chStore, err := clickhouse.NewClickHouseStore(cfg, tracer)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	clients, err := postgres.NewEntClients(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	rp := repository.RepositoryParams{
		Logger:        log,
		EntClient:     postgres.NewClient(clients, log, tracer),
		ClickHouseDB:  chStore,
		InMemoryCache: cache.NewInMemoryCache(),
		RedisCache:    cache.NewRedisCache(),
	}
	webhooks, err := publisher.NewPublisher(memory.NewPubSub(cfg, log), cfg, log, nil)
	if err != nil {
		return nil, err
	}
	encryption, err := security.NewEncryptionService(cfg, log)
	if err != nil {
		return nil, err
	}
	connections := repository.NewConnectionRepository(rp)
	customers := repository.NewCustomerRepository(rp)
	subscriptions := repository.NewSubscriptionRepository(rp)
	plans := repository.NewPlanRepository(rp)
	invoices := repository.NewInvoiceRepository(rp)
	payments := repository.NewPaymentRepository(rp)
	prices := repository.NewPriceRepository(rp)
	mappings := repository.NewEntityIntegrationMappingRepository(rp)
	meters := repository.NewMeterRepository(rp)
	features := repository.NewFeatureRepository(rp)
	settings := repository.NewSettingsRepository(rp)
	// Integrations are looked up per call; with no published connection every
	// provider resolves to "not configured", which is what the hooks expect.
	integrations := integration.NewFactory(cfg, log, connections, customers, subscriptions, plans, invoices,
		payments, repository.NewPaymentMethodRepository(rp), prices, mappings, meters, features,
		encryption, nil, nil, settings)
	params := service.ServiceParams{
		IntegrationFactory:           integrations,
		EncryptionService:            encryption,
		Logger:                       log,
		Config:                       cfg,
		DB:                           rp.EntClient,
		TracingSvc:                   tracer,
		InMemoryCache:                rp.InMemoryCache,
		RedisCache:                   rp.RedisCache,
		WebhookPublisher:             webhooks,
		UserRepo:                     repository.NewUserRepository(rp),
		EventRepo:                    repository.NewEventRepository(rp),
		ProcessedEventRepo:           repository.NewProcessedEventRepository(rp),
		MeterUsageRepo:               repository.NewMeterUsageRepository(rp),
		MeterRepo:                    repository.NewMeterRepository(rp),
		PriceRepo:                    repository.NewPriceRepository(rp),
		PriceUnitRepo:                repository.NewPriceUnitRepository(rp),
		CustomerRepo:                 repository.NewCustomerRepository(rp),
		PlanRepo:                     repository.NewPlanRepository(rp),
		PlanPriceSyncRepo:            repository.NewPlanPriceSyncRepository(rp),
		AddonPriceSyncRepo:           repository.NewAddonPriceSyncRepository(rp),
		SubRepo:                      repository.NewSubscriptionRepository(rp),
		SubscriptionLineItemRepo:     repository.NewSubscriptionLineItemRepository(rp),
		SubscriptionPhaseRepo:        repository.NewSubscriptionPhaseRepository(rp),
		SubScheduleRepo:              repository.NewSubscriptionScheduleRepository(rp),
		WalletRepo:                   repository.NewWalletRepository(rp),
		TenantRepo:                   repository.NewTenantRepository(rp),
		EnvironmentRepo:              repository.NewEnvironmentRepository(rp),
		InvoiceRepo:                  repository.NewInvoiceRepository(rp),
		InvoiceLineItemRepo:          repository.NewInvoiceLineItemRepository(rp),
		FeatureRepo:                  repository.NewFeatureRepository(rp),
		EntitlementRepo:              repository.NewEntitlementRepository(rp),
		EntitlementGrantRepo:         repository.NewEntitlementGrantRepository(rp),
		CreditGrantRepo:              repository.NewCreditGrantRepository(rp),
		CostSheetRepo:                repository.NewCostsheetRepository(rp),
		AddonRepo:                    repository.NewAddonRepository(rp),
		AddonAssociationRepo:         repository.NewAddonAssociationRepository(rp),
		ConnectionRepo:               repository.NewConnectionRepository(rp),
		SettingsRepo:                 repository.NewSettingsRepository(rp),
		GroupRepo:                    repository.NewGroupRepository(rp),
		ProductRepo:                  repository.NewProductRepository(rp),
		TaxRateRepo:                  repository.NewTaxRateRepository(rp),
		TaxAssociationRepo:           repository.NewTaxAssociationRepository(rp),
		FXRateRepo:                   repository.NewFXRateRepository(rp),
		CouponRepo:                   repository.NewCouponRepository(rp),
		CouponAssociationRepo:        repository.NewCouponAssociationRepository(rp),
		CouponApplicationRepo:        repository.NewCouponApplicationRepository(rp),
		ScheduledTaskRepo:            repository.NewScheduledTaskRepository(rp),
		CommitmentRepo:               repository.NewCommitmentRepository(rp),
		CheckoutSessionRepo:          repository.NewCheckoutSessionRepository(rp),
		PaymentRepo:                  repository.NewPaymentRepository(rp),
		RefundRepo:                   repository.NewRefundRepository(rp),
		SecretRepo:                   repository.NewSecretRepository(rp),
		TaskRepo:                     repository.NewTaskRepository(rp),
		CreditNoteRepo:               repository.NewCreditNoteRepository(rp),
		CreditNoteLineItemRepo:       repository.NewCreditNoteLineItemRepository(rp),
		CreditGrantApplicationRepo:   repository.NewCreditGrantApplicationRepository(rp),
		TaxAppliedRepo:               repository.NewTaxAppliedRepository(rp),
		HostedInvoiceTokenRepo:       repository.NewHostedInvoiceTokenRepository(rp),
		EntityIntegrationMappingRepo: mappings,
		UsageRecordRepo:              repository.NewUsageRecordRepository(rp),
		AlertLogsRepo:                repository.NewAlertLogsRepository(rp),
		AlertRepo:                    repository.NewAlertSettingsRepository(rp),
		WorkflowExecutionRepo:        repository.NewWorkflowExecutionRepository(rp),
		MeterUsageCheckpointRepo:     repository.NewMeterUsageCheckpointRepository(rp),
		RawEventRepo:                 repository.NewRawEventRepository(rp),
		AuthRepo:                     repository.NewAuthRepository(rp),
		ActivityRepo:                 repository.NewActivityRepository(rp),
		RevenueFactRepo:              repository.NewRevenueFactRepository(rp),
	}
	return &dbWriter{
		eventRepo:      params.EventRepo,
		meterUsageRepo: params.MeterUsageRepo,
		meterRepo:      meters,
		customers:      service.NewCustomerService(params),
		features:       service.NewFeatureService(params),
		plans:          service.NewPlanService(params),
		prices:         service.NewPriceService(params),
		entitlements:   service.NewEntitlementService(params),
		subscriptions:  service.NewSubscriptionService(params),
		invoices:       service.NewInvoiceService(params),
		creditNotes:    service.NewCreditNoteService(params),
		settings:       service.NewSettingsService(params),
		invoiceRepo:    params.InvoiceRepo,
		priceRepo:      params.PriceRepo,
		subRepo:        params.SubRepo,
		client:         rp.EntClient,
	}, nil
}

// scopedContext carries the tenant, environment and acting user the services
// read for every query and every created_by column.
func scopedContext(ctx context.Context, tenantID, envID, userID string) context.Context {
	ctx = context.WithValue(ctx, types.CtxTenantID, tenantID)
	ctx = context.WithValue(ctx, types.CtxEnvironmentID, envID)
	ctx = context.WithValue(ctx, types.CtxUserID, userID)
	return ctx
}

// convert moves a request map or a service response through JSON into the
// target type, so both writers share the same wire-shaped payloads.
func convert(in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (w *dbWriter) customerByExternalID(ctx context.Context, externalID string) (*customerRecord, error) {
	resp, err := w.customers.GetCustomerByLookupKey(ctx, externalID)
	if ierr.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c customerRecord
	return &c, convert(resp, &c)
}

func (w *dbWriter) createCustomer(ctx context.Context, req map[string]any) (*customerRecord, error) {
	var in dto.CreateCustomerRequest
	if err := convert(req, &in); err != nil {
		return nil, err
	}
	resp, err := w.customers.CreateCustomer(ctx, in)
	if err != nil {
		return nil, err
	}
	var c customerRecord
	return &c, convert(resp, &c)
}

func (w *dbWriter) featuresByLookupKeys(ctx context.Context, keys []string) (map[string]featureRecord, error) {
	filter := &types.FeatureFilter{QueryFilter: types.NewNoLimitQueryFilter(), LookupKeys: keys}
	resp, err := w.features.GetFeatures(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := map[string]featureRecord{}
	for _, item := range resp.Items {
		var f featureRecord
		if err := convert(item, &f); err != nil {
			return nil, err
		}
		out[f.LookupKey] = f
	}
	return out, nil
}

func (w *dbWriter) createFeature(ctx context.Context, req map[string]any) (*featureRecord, error) {
	var in dto.CreateFeatureRequest
	if err := convert(req, &in); err != nil {
		return nil, err
	}
	resp, err := w.features.CreateFeature(ctx, in)
	if err != nil {
		return nil, err
	}
	var f featureRecord
	return &f, convert(resp, &f)
}

func (w *dbWriter) planByLookupKey(ctx context.Context, key string) (*planRecord, error) {
	filter := types.NewPlanFilter()
	filter.LookupKey = lo.ToPtr(key)
	list, err := w.plans.GetPlans(ctx, filter)
	if err != nil {
		return nil, err
	}
	for _, item := range list.Items {
		if item.Plan == nil || item.LookupKey != key {
			continue
		}
		full, err := w.plans.GetPlan(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		var p planRecord
		return &p, convert(full, &p)
	}
	return nil, nil
}

func (w *dbWriter) createPlan(ctx context.Context, req map[string]any) (*planRecord, error) {
	var in dto.CreatePlanRequest
	if err := convert(req, &in); err != nil {
		return nil, err
	}
	resp, err := w.plans.CreatePlan(ctx, in)
	if err != nil {
		return nil, err
	}
	var p planRecord
	return &p, convert(resp, &p)
}

func (w *dbWriter) createPrice(ctx context.Context, req map[string]any) (*priceRecord, error) {
	var in dto.CreatePriceRequest
	if err := convert(req, &in); err != nil {
		return nil, err
	}
	resp, err := w.prices.CreatePrice(ctx, in)
	if err != nil {
		return nil, err
	}
	var p priceRecord
	return &p, convert(resp, &p)
}

func (w *dbWriter) createEntitlement(ctx context.Context, req map[string]any) error {
	var in dto.CreateEntitlementRequest
	if err := convert(req, &in); err != nil {
		return err
	}
	_, err := w.entitlements.CreateEntitlement(ctx, in)
	return err
}

func (w *dbWriter) subscriptionsForCustomer(ctx context.Context, externalID string) ([]subscriptionRecord, error) {
	resp, err := w.subscriptions.GetSubscriptionsForCustomer(ctx, externalID, types.NewExpand(""))
	if ierr.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []subscriptionRecord
	return out, convert(resp.Items, &out)
}

func (w *dbWriter) createSubscription(ctx context.Context, req map[string]any) (*subscriptionRecord, error) {
	var in dto.CreateSubscriptionRequest
	if err := convert(req, &in); err != nil {
		return nil, err
	}
	resp, err := w.subscriptions.CreateSubscription(ctx, in)
	if err != nil {
		return nil, err
	}
	var s subscriptionRecord
	return &s, convert(resp, &s)
}

func (w *dbWriter) deletePlan(ctx context.Context, id string) error {
	return w.plans.DeletePlan(ctx, id)
}

func (w *dbWriter) deleteCustomer(ctx context.Context, id string) error {
	return w.customers.DeleteCustomer(ctx, id)
}

func (w *dbWriter) updatePlan(ctx context.Context, id string, req map[string]any) error {
	var in dto.UpdatePlanRequest
	if err := convert(req, &in); err != nil {
		return err
	}
	_, err := w.plans.UpdatePlan(ctx, id, in)
	return err
}

func (w *dbWriter) updateCustomer(ctx context.Context, id string, req map[string]any) error {
	var in dto.UpdateCustomerRequest
	if err := convert(req, &in); err != nil {
		return err
	}
	_, err := w.customers.UpdateCustomer(ctx, id, in)
	return err
}

func (w *dbWriter) cancelSubscription(ctx context.Context, id string, req map[string]any) error {
	var in dto.CancelSubscriptionRequest
	if err := convert(req, &in); err != nil {
		return err
	}
	_, err := w.subscriptions.CancelSubscription(ctx, id, &in)
	return err
}

func (w *dbWriter) syncPlanPrices(ctx context.Context, planID string) (int, error) {
	resp, err := w.plans.SyncPlanPricesV2(ctx, planID)
	if err != nil {
		return 0, err
	}
	return resp.Summary.LineItemsCreated, nil
}

// ingestEvents writes the events and the meter_usage rows the Kafka tracking
// service would derive from them, using the same filter, quantity and
// unique-hash rules, so invoices computed from meter_usage see this usage.
func (w *dbWriter) ingestEvents(ctx context.Context, batch []*events.Event) error {
	if len(batch) == 0 {
		return nil
	}
	metersByEvent := map[string][]*meter.Meter{}
	now := time.Now().UTC()
	records := make([]*events.MeterUsage, 0, len(batch))
	for _, e := range batch {
		e.IngestedAt = now
		ms, ok := metersByEvent[e.EventName]
		if !ok {
			var err error
			ms, err = w.meterRepo.GetMatchingMetersByEventName(ctx, e.EventName)
			if err != nil {
				return fmt.Errorf("meters for %s: %w", e.EventName, err)
			}
			metersByEvent[e.EventName] = ms
		}
		for _, m := range ms {
			if !meterFiltersMatch(e, m.Filters) {
				continue
			}
			qty, err := meterQuantity(e, m)
			if err != nil {
				return err
			}
			records = append(records, &events.MeterUsage{
				Event:      *e,
				MeterID:    m.ID,
				QtyTotal:   qty,
				UniqueHash: meterUniqueHash(e, m),
			})
		}
	}
	if err := w.eventRepo.BulkInsertEvents(ctx, batch); err != nil {
		return fmt.Errorf("insert events: %w", err)
	}
	if err := w.meterUsageRepo.BulkInsertMeterUsage(ctx, records); err != nil {
		return fmt.Errorf("insert meter usage: %w", err)
	}
	return nil
}

func meterFiltersMatch(e *events.Event, filters []meter.Filter) bool {
	for _, f := range filters {
		v, ok := e.Properties[f.Key]
		if !ok || !lo.Contains(f.Values, fmt.Sprintf("%v", v)) {
			return false
		}
	}
	return true
}

// meterQuantity supports the aggregations this catalog uses: COUNT,
// COUNT_UNIQUE and SUM. Anything else is refused rather than guessed.
func meterQuantity(e *events.Event, m *meter.Meter) (decimal.Decimal, error) {
	if m.Aggregation.Expression != "" {
		return decimal.Zero, fmt.Errorf("meter %s uses a CEL expression; ingest through the API instead", m.ID)
	}
	switch m.Aggregation.Type {
	case types.AggregationCount:
		return decimal.NewFromInt(1), nil
	case types.AggregationCountUnique:
		if _, ok := e.Properties[m.Aggregation.Field]; !ok {
			return decimal.Zero, nil
		}
		return decimal.NewFromInt(1), nil
	case types.AggregationSum:
		v, ok := e.Properties[m.Aggregation.Field]
		if !ok {
			return decimal.Zero, nil
		}
		return decimal.NewFromString(fmt.Sprintf("%v", v))
	default:
		return decimal.Zero, fmt.Errorf("meter %s aggregation %s not supported by direct ingest", m.ID, m.Aggregation.Type)
	}
}

func meterUniqueHash(e *events.Event, m *meter.Meter) string {
	if m.Aggregation.Type != types.AggregationCountUnique {
		return ""
	}
	v, ok := e.Properties[m.Aggregation.Field]
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%v", e.EventName, m.Aggregation.Field, v)))
	return hex.EncodeToString(sum[:])
}

func (w *dbWriter) updateBillingPeriods(ctx context.Context) (*dto.SubscriptionUpdatePeriodResponse, error) {
	return w.subscriptions.UpdateBillingPeriods(ctx)
}

func (w *dbWriter) usageForSubscription(ctx context.Context, subscriptionID string, start, end time.Time) (*dto.GetUsageBySubscriptionResponse, error) {
	return w.subscriptions.GetMeterUsageBySubscription(ctx, &dto.GetUsageBySubscriptionRequest{
		SubscriptionID: subscriptionID,
		StartTime:      start,
		EndTime:        end,
		Source:         string(types.UsageSourceInvoiceCreation),
	})
}

func (w *dbWriter) terminatePrice(ctx context.Context, id string, end time.Time) error {
	return w.prices.DeletePrice(ctx, id, dto.DeletePriceRequest{EndDate: lo.ToPtr(end.UTC())})
}

func (w *dbWriter) invoicesForSubscription(ctx context.Context, subscriptionID string) ([]invoiceRecord, error) {
	f := types.NewNoLimitInvoiceFilter()
	f.SubscriptionID = subscriptionID
	resp, err := w.invoices.ListInvoices(ctx, f)
	if err != nil {
		return nil, err
	}
	var out []invoiceRecord
	return out, convert(resp.Items, &out)
}

func (w *dbWriter) invoice(ctx context.Context, id string) (*invoiceRecord, error) {
	resp, err := w.invoices.GetInvoice(ctx, id)
	if err != nil {
		return nil, err
	}
	var out invoiceRecord
	return &out, convert(resp, &out)
}

func (w *dbWriter) creditNotesForInvoice(ctx context.Context, invoiceID string) ([]creditNoteRecord, error) {
	f := types.NewNoLimitCreditNoteFilter()
	f.InvoiceID = invoiceID
	resp, err := w.creditNotes.ListCreditNotes(ctx, f)
	if err != nil {
		return nil, err
	}
	var out []creditNoteRecord
	return out, convert(resp.Items, &out)
}

func (w *dbWriter) createCreditNote(ctx context.Context, req map[string]any) (*creditNoteRecord, error) {
	var in dto.CreateCreditNoteRequest
	if err := convert(req, &in); err != nil {
		return nil, err
	}
	resp, err := w.creditNotes.CreateCreditNote(ctx, &in)
	if err != nil {
		return nil, err
	}
	var out creditNoteRecord
	return &out, convert(resp, &out)
}

func (w *dbWriter) archivePrice(ctx context.Context, id string) error {
	n, err := w.client.Writer(ctx).SubscriptionLineItem.Query().
		Where(subscriptionlineitem.PriceID(id), subscriptionlineitem.TenantID(types.GetTenantID(ctx))).
		Count(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("price %s is on %d subscription line items", id, n)
	}
	return w.priceRepo.Delete(ctx, id)
}

func (w *dbWriter) planPrices(ctx context.Context, planID string) ([]priceRecord, error) {
	resp, err := w.prices.GetPricesByPlanID(ctx, dto.GetPricesByPlanRequest{PlanID: planID, AllowExpired: true})
	if err != nil {
		return nil, err
	}
	var out []priceRecord
	return out, convert(resp.Items, &out)
}

func (w *dbWriter) cancelledSubscriptions(ctx context.Context, customerID string) ([]subscriptionRecord, error) {
	resp, err := w.subscriptions.ListSubscriptions(ctx, &types.SubscriptionFilter{
		QueryFilter:        types.NewNoLimitQueryFilter(),
		CustomerID:         customerID,
		SubscriptionStatus: []types.SubscriptionStatus{types.SubscriptionStatusCancelled},
	})
	if err != nil {
		return nil, err
	}
	var out []subscriptionRecord
	return out, convert(resp.Items, &out)
}

func (w *dbWriter) listInvoices(ctx context.Context, status string) ([]invoiceRecord, error) {
	f := types.NewNoLimitInvoiceFilter()
	if status != "" {
		f.InvoiceStatus = []types.InvoiceStatus{types.InvoiceStatus(status)}
	}
	resp, err := w.invoices.ListInvoices(ctx, f)
	if err != nil {
		return nil, err
	}
	var out []invoiceRecord
	return out, convert(resp.Items, &out)
}

func (w *dbWriter) invoiceConfig(ctx context.Context) (types.InvoiceConfig, error) {
	resp, err := w.settings.GetSettingByKey(ctx, types.SettingKeyInvoiceConfig)
	if err != nil {
		return types.InvoiceConfig{}, err
	}
	return utils.ToStruct[types.InvoiceConfig](resp.Value)
}

func (w *dbWriter) invoiceDueDate(ctx context.Context, subscriptionID string, issue time.Time) (time.Time, error) {
	sub, err := w.subRepo.Get(ctx, subscriptionID)
	if err != nil {
		return time.Time{}, err
	}
	cfg, err := w.invoiceConfig(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if loc, err := time.LoadLocation(sub.Timezone); err == nil {
		issue = issue.In(loc)
	}
	due, ok := types.PaymentTermsDueDate(lo.FromPtr(sub.PaymentTerms), issue)
	if !ok {
		due, ok = types.PaymentTermsDueDate(lo.FromPtr(cfg.PaymentTerms), issue)
	}
	if !ok {
		due = issue.AddDate(0, 0, lo.FromPtr(cfg.DueDateDays))
	}
	return due.UTC(), nil
}

func (w *dbWriter) redateInvoice(ctx context.Context, id, number string, issue, due time.Time) error {
	inv, err := w.invoiceRepo.Get(ctx, id)
	if err != nil {
		return err
	}
	inv.InvoiceNumber = &number
	inv.IssueDate = &issue
	inv.DueDate = &due
	return w.invoiceRepo.Update(ctx, inv)
}

func (w *dbWriter) setInvoiceSequence(ctx context.Context, yearMonth string, last int64) error {
	const query = `
		INSERT INTO invoice_sequences (tenant_id, environment_id, year_month, last_value, created_at, updated_at)
		VALUES ($1, $2, $3, $4, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT (tenant_id, environment_id, year_month) DO UPDATE
		SET last_value = EXCLUDED.last_value, updated_at = CURRENT_TIMESTAMP`
	_, err := w.client.Writer(ctx).ExecContext(ctx, query, types.GetTenantID(ctx), types.GetEnvironmentID(ctx), yearMonth, last)
	return err
}

func (w *dbWriter) purgeInvoices(ctx context.Context, ids []string) (int, error) {
	var deleted []*ent.Invoice
	err := w.client.WithTx(ctx, func(txCtx context.Context) error {
		tx := w.client.Writer(txCtx)
		noteIDs, err := tx.CreditNote.Query().
			Where(creditnote.InvoiceIDIn(ids...), creditnote.TenantID(types.GetTenantID(txCtx))).
			IDs(txCtx)
		if err != nil {
			return err
		}
		if len(noteIDs) > 0 {
			if _, err := tx.CreditNoteLineItem.Delete().
				Where(creditnotelineitem.CreditNoteIDIn(noteIDs...), creditnotelineitem.TenantID(types.GetTenantID(txCtx))).
				ExecReturning(txCtx); err != nil {
				return err
			}
			if _, err := tx.CreditNote.Delete().
				Where(creditnote.IDIn(noteIDs...), creditnote.TenantID(types.GetTenantID(txCtx))).
				ExecReturning(txCtx); err != nil {
				return err
			}
		}
		if _, err := tx.InvoiceLineItem.Delete().
			Where(invoicelineitem.InvoiceIDIn(ids...), invoicelineitem.TenantID(types.GetTenantID(txCtx)),
				invoicelineitem.EnvironmentID(types.GetEnvironmentID(txCtx))).
			ExecReturning(txCtx); err != nil {
			return err
		}
		deleted, err = tx.Invoice.Delete().
			Where(entinvoice.IDIn(ids...), entinvoice.TenantID(types.GetTenantID(txCtx)),
				entinvoice.EnvironmentID(types.GetEnvironmentID(txCtx))).
			ExecReturning(txCtx)
		return err
	})
	return len(deleted), err
}
