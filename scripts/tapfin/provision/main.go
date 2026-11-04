// Command provision maps evision organisations to Flexprice customers and
// creates one FleetForce plan per organisation from a rate card.
//
// Dry-run by default; pass -apply to write. Reads CLICKHOUSE_EVISION_* and
// FLEXPRICE_API_URL / FLEXPRICE_API_KEY / FLEXPRICE_ENVIRONMENT_ID from the
// environment or -env-file.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/joho/godotenv"
	"github.com/samber/lo"
)

type options struct {
	envFile    string
	rateCard   string
	orgs       string
	exclude    string
	types      string
	rollback   bool
	resub      bool
	emitAssets bool
	periods    bool
	usage      bool
	prorate    bool
	purgeZero  bool
	purgeOld   bool
	redate     bool
	from       string
	to         string
	statuses   string
	apply      bool
	subscribe  bool
	startDate  string
	timezone   string
	emailTmpl  string
	listOrgs   bool
	mode       string
	tenantID   string
	envID      string
	userID     string
}

func main() {
	var opt options
	flag.StringVar(&opt.envFile, "env-file", "", "dotenv file with evision ClickHouse and Flexprice credentials")
	flag.StringVar(&opt.rateCard, "ratecard", "scripts/tapfin/provision/ratecard.json", "rate card JSON")
	flag.StringVar(&opt.orgs, "orgs", "", "comma-separated org codes or name substrings to limit the run")
	flag.StringVar(&opt.exclude, "exclude", "", "comma-separated org codes or name substrings to leave untouched")
	flag.BoolVar(&opt.resub, "resubscribe", false, "with -subscribe: cancel the org's existing active subscription (immediately, no invoice) and create a fresh one")
	flag.BoolVar(&opt.emitAssets, "emit-assets", false, "generate daily asset_billing_state events from evision asset_x_organization for the selected orgs")
	flag.StringVar(&opt.from, "from", "", "emit-assets: first day YYYY-MM-DD (default: first day of the current month)")
	flag.StringVar(&opt.to, "to", "", "emit-assets: last day YYYY-MM-DD inclusive (default: today)")
	flag.StringVar(&opt.statuses, "asset-statuses", defaultBillableStatuses, "emit-assets: onboarding statuses that are billable")
	flag.BoolVar(&opt.usage, "preview-usage", false, "price the selected orgs' metered usage for -from..-to (default: the current month) as invoice creation would; writes nothing")
	flag.BoolVar(&opt.prorate, "prorate-first-period", false, "credit back the unserved days of each selected org's first-period invoice with an adjustment credit note (after -update-periods)")
	flag.BoolVar(&opt.purgeZero, "purge-zero-invoices", false, "delete every zero-total SKIPPED invoice in the environment (db mode, all customers)")
	flag.BoolVar(&opt.purgeOld, "purge-cancelled-invoices", false, "delete every invoice and credit note of the selected orgs' cancelled subscriptions (db mode)")
	flag.BoolVar(&opt.redate, "redate-invoices", false, "renumber and redate every finalized invoice in the environment as if raised at its period end (db mode, all customers)")
	flag.BoolVar(&opt.periods, "update-periods", false, "run the subscription billing-period update: every active subscription in the environment is rolled to the current period and each elapsed period is invoiced")
	flag.BoolVar(&opt.rollback, "rollback", false, "archive the selected organisations' plans and customers instead of creating them")
	flag.StringVar(&opt.types, "types", "FLEET_OPERATOR,LENDER", "comma-separated evision org types to include, or 'all'")
	flag.BoolVar(&opt.apply, "apply", false, "write to Flexprice (default is a dry run)")
	flag.BoolVar(&opt.subscribe, "subscribe", false, "also create a monthly subscription per organisation")
	flag.StringVar(&opt.startDate, "start-date", "", "subscription start date YYYY-MM-DD for every org (default: the day the org's first billable asset was onboarded, else the first day of the current month)")
	flag.StringVar(&opt.timezone, "timezone", "Asia/Kolkata", "subscription timezone")
	flag.StringVar(&opt.emailTmpl, "email-template", "", "email set on customers that have none, e.g. tech+{code}@tapfin.in ({code} is the lower-cased org code)")
	flag.BoolVar(&opt.listOrgs, "list-orgs", false, "only print the evision organisations and their resolved rate card")
	flag.StringVar(&opt.mode, "mode", "rest", "rest: Flexprice HTTP API via FLEXPRICE_API_URL/KEY; db: service layer on the FLEXPRICE_* database config")
	flag.StringVar(&opt.tenantID, "tenant", "", "db mode: tenant id")
	flag.StringVar(&opt.envID, "environment", "", "db mode: environment id")
	flag.StringVar(&opt.userID, "user", "", "db mode: user id recorded as creator")
	flag.Parse()

	if err := run(context.Background(), opt); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, opt options) error {
	if opt.envFile != "" {
		if err := godotenv.Load(opt.envFile); err != nil {
			return fmt.Errorf("load %s: %w", opt.envFile, err)
		}
	}
	book, err := loadRateBook(opt.rateCard)
	if err != nil {
		return err
	}

	conn, db, err := openEvision()
	if err != nil {
		return err
	}
	defer conn.Close()
	orgs, err := fetchOrganizations(ctx, conn, db)
	if err != nil {
		return err
	}
	total := len(orgs)
	orgs = excludeOrgs(filterOrgs(filterTypes(orgs, opt.types), opt.orgs), opt.exclude)
	fmt.Printf("evision: %d organisations in table\n", total)
	fmt.Printf("evision: %d organisations selected\n", len(orgs))
	if len(orgs) == 0 || opt.listOrgs {
		for _, org := range orgs {
			_, cardKey := book.resolve(org)
			fmt.Printf("  %-12s %-40s type=%-12s rate_card=%s\n", org.Code, org.Name, org.Type, cardKey)
		}
		return nil
	}

	api, ctx, err := openWriter(ctx, opt)
	if err != nil {
		return err
	}
	p := &provisioner{api: api, book: book, opt: opt}
	if opt.subscribe && opt.startDate == "" && !opt.rollback {
		p.firstAsset, err = fetchFirstOnboarding(ctx, conn, db, orgCodes(orgs), p.statuses())
		if err != nil {
			return err
		}
	}
	if opt.emitAssets {
		return p.emitAssetEvents(ctx, conn, db, orgs)
	}
	if opt.periods {
		return p.updateBillingPeriods(ctx, orgs)
	}
	if opt.purgeZero {
		return p.purgeZeroInvoices(ctx)
	}
	if opt.purgeOld {
		return p.purgeCancelledInvoices(ctx, orgs)
	}
	if opt.redate {
		return p.redateInvoices(ctx)
	}
	if opt.usage {
		return p.previewUsage(ctx, orgs)
	}
	if opt.prorate {
		if err := p.prorateFirstPeriods(ctx, orgs); err != nil {
			return err
		}
		fmt.Printf("\ncredit notes: %d\n", p.stats.creditNotes)
		return nil
	}
	if opt.rollback {
		for _, org := range orgs {
			if err := p.rollbackOrg(ctx, org); err != nil {
				return fmt.Errorf("org %s (%s): %w", org.Code, org.Name, err)
			}
		}
		p.summary()
		return nil
	}
	if err := p.ensureCatalog(ctx); err != nil {
		return err
	}
	for _, org := range orgs {
		if err := p.provisionOrg(ctx, org); err != nil {
			return fmt.Errorf("org %s (%s): %w", org.Code, org.Name, err)
		}
	}
	p.summary()
	return nil
}

func excludeOrgs(orgs []organization, selector string) []organization {
	if strings.TrimSpace(selector) == "" {
		return orgs
	}
	drop := map[string]bool{}
	for _, o := range filterOrgs(orgs, selector) {
		drop[o.Code] = true
	}
	var out []organization
	for _, o := range orgs {
		if !drop[o.Code] {
			out = append(out, o)
		}
	}
	return out
}

func openWriter(ctx context.Context, opt options) (writer, context.Context, error) {
	switch opt.mode {
	case "rest":
		api, err := newFlexprice()
		return api, ctx, err
	case "db":
		if opt.tenantID == "" || opt.envID == "" || opt.userID == "" {
			return nil, ctx, fmt.Errorf("db mode needs -tenant, -environment and -user")
		}
		api, err := newDBWriter()
		if err != nil {
			return nil, ctx, err
		}
		return api, scopedContext(ctx, opt.tenantID, opt.envID, opt.userID), nil
	default:
		return nil, ctx, fmt.Errorf("unknown -mode %q", opt.mode)
	}
}

func filterTypes(orgs []organization, types string) []organization {
	types = strings.TrimSpace(types)
	if types == "" || strings.EqualFold(types, "all") {
		return orgs
	}
	allowed := map[string]bool{}
	for _, t := range strings.Split(types, ",") {
		allowed[strings.ToUpper(strings.TrimSpace(t))] = true
	}
	var out []organization
	for _, o := range orgs {
		if allowed[strings.ToUpper(o.Type)] {
			out = append(out, o)
		}
	}
	return out
}

func filterOrgs(orgs []organization, selector string) []organization {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return orgs
	}
	var out []organization
	for _, o := range orgs {
		for _, term := range strings.Split(selector, ",") {
			term = strings.ToLower(strings.TrimSpace(term))
			if term == "" {
				continue
			}
			if strings.ToLower(o.Code) == term || strings.Contains(strings.ToLower(o.Name), term) {
				out = append(out, o)
				break
			}
		}
	}
	return out
}

// emitAssetEvents turns the orgs' asset links into daily asset_billing_state
// events for the window and ingests them in batches.
func (p *provisioner) emitAssetEvents(ctx context.Context, conn driver.Conn, db string, orgs []organization) error {
	loc, err := time.LoadLocation(p.opt.timezone)
	if err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	from, to, err := p.assetWindow(loc)
	if err != nil {
		return err
	}
	statuses := p.statuses()
	links, err := fetchAssetLinks(ctx, conn, db, orgCodes(orgs), statuses)
	if err != nil {
		return err
	}
	classifyThreeWheelers(links, p.book)
	evts := assetStateEvents(links, from, to, loc, p.opt.tenantID, p.opt.envID)
	fmt.Printf("asset links: %d billable (statuses %s); window %s..%s; events: %d\n",
		len(links), strings.Join(statuses, ","), from.Format("2006-01-02"), to.Format("2006-01-02"), len(evts))

	fmt.Printf("\n%-12s %-8s %-16s %-12s %8s %10s\n", "org", "month", "form_factor", "state", "assets", "asset_days")
	for _, s := range summarizeAssetEvents(evts, loc) {
		fmt.Printf("%-12s %-8s %-16s %-12s %8d %10d\n", s.Org, s.Month, s.FormFactor, s.State, len(s.Assets), s.AssetDays)
	}
	if !p.write() {
		fmt.Printf("\n[dry-run] would ingest %d events\n", len(evts))
		return nil
	}
	const batch = 2000
	for start := 0; start < len(evts); start += batch {
		end := start + batch
		if end > len(evts) {
			end = len(evts)
		}
		if err := p.api.ingestEvents(ctx, evts[start:end]); err != nil {
			return fmt.Errorf("ingest events %d..%d: %w", start, end, err)
		}
		fmt.Printf("ingested %d/%d\r", end, len(evts))
	}
	fmt.Printf("\ningested %d events\n", len(evts))
	return nil
}

func (p *provisioner) previewUsage(ctx context.Context, orgs []organization) error {
	loc, err := time.LoadLocation(p.opt.timezone)
	if err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	from, to, err := p.assetWindow(loc)
	if err != nil {
		return err
	}
	end := to.AddDate(0, 0, 1)
	for _, org := range orgs {
		subs, err := p.api.subscriptionsForCustomer(ctx, org.Code)
		if err != nil {
			return fmt.Errorf("org %s: list subscriptions: %w", org.Code, err)
		}
		for _, s := range subs {
			if s.SubscriptionStatus != "active" {
				continue
			}
			usage, err := p.api.usageForSubscription(ctx, s.ID, from, end)
			if err != nil {
				return fmt.Errorf("org %s: usage for %s: %w", org.Code, s.ID, err)
			}
			fmt.Printf("\n%s %s usage %s..%s: %s %s\n", org.Code, s.ID, from.Format("2006-01-02"), end.Format("2006-01-02"), usage.DisplayAmount, usage.Currency)
			for _, c := range usage.Charges {
				fmt.Printf("  %-28s qty %12.2f  %s\n", c.MeterDisplayName, c.Quantity, c.DisplayAmount)
			}
		}
	}
	return nil
}

// updateBillingPeriods invoices elapsed periods. The service rolls every
// active subscription in the environment, not only the selected orgs, so the
// dry run lists the selected orgs' subscriptions that are behind.
func (p *provisioner) updateBillingPeriods(ctx context.Context, orgs []organization) error {
	now := time.Now()
	for _, org := range orgs {
		subs, err := p.api.subscriptionsForCustomer(ctx, org.Code)
		if err != nil {
			return fmt.Errorf("org %s: list subscriptions: %w", org.Code, err)
		}
		for _, s := range subs {
			if s.SubscriptionStatus != "active" {
				continue
			}
			state := "current"
			if s.CurrentPeriodEnd.Before(now) {
				state = "behind"
			}
			fmt.Printf("%-12s %s period %s..%s %s\n", org.Code, s.ID,
				s.CurrentPeriodStart.Format("2006-01-02"), s.CurrentPeriodEnd.Format("2006-01-02"), state)
		}
	}
	if !p.write() {
		fmt.Println("\n[dry-run] -apply rolls every active subscription in the environment forward and invoices each elapsed period")
		return nil
	}
	resp, err := p.api.updateBillingPeriods(ctx)
	if err != nil {
		return err
	}
	for _, it := range resp.Items {
		status := "ok"
		if !it.Success {
			status = "FAILED " + it.Error
		}
		fmt.Printf("%s %s..%s %s\n", it.SubscriptionID, it.PeriodStart.Format("2006-01-02"), it.PeriodEnd.Format("2006-01-02"), status)
	}
	fmt.Printf("\nperiod update: %d succeeded, %d failed\n", resp.TotalSuccess, resp.TotalFailed)
	if resp.TotalFailed > 0 {
		return fmt.Errorf("%d subscriptions failed", resp.TotalFailed)
	}
	return nil
}

func orgCodes(orgs []organization) []string {
	codes := make([]string, 0, len(orgs))
	for _, o := range orgs {
		codes = append(codes, o.Code)
	}
	return codes
}

func (p *provisioner) statuses() []string {
	return strings.Split(strings.ToUpper(strings.ReplaceAll(p.opt.statuses, " ", "")), ",")
}

func (p *provisioner) assetWindow(loc *time.Location) (time.Time, time.Time, error) {
	now := time.Now().In(loc)
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	var err error
	if p.opt.from != "" {
		if from, err = time.ParseInLocation("2006-01-02", p.opt.from, loc); err != nil {
			return from, to, fmt.Errorf("-from: %w", err)
		}
	}
	if p.opt.to != "" {
		if to, err = time.ParseInLocation("2006-01-02", p.opt.to, loc); err != nil {
			return from, to, fmt.Errorf("-to: %w", err)
		}
	}
	if to.Before(from) {
		return from, to, fmt.Errorf("-to is before -from")
	}
	return from, to, nil
}

// rollbackOrg archives the organisation's plan and customer. Shared features
// and meters stay; other plans still use them.
func (p *provisioner) rollbackOrg(ctx context.Context, org organization) error {
	fmt.Printf("\n== %s  code=%s\n", org.Name, org.Code)
	card, _ := p.book.resolve(org)
	plan, err := p.api.planByLookupKey(ctx, org.planLookupKey(card.Product))
	if err != nil {
		return fmt.Errorf("lookup plan: %w", err)
	}
	if plan != nil {
		p.logf("archive plan %s (%s)", plan.ID, plan.Name)
		p.stats.plansArchived++
		if p.write() {
			if err := p.api.deletePlan(ctx, plan.ID); err != nil {
				return fmt.Errorf("archive plan: %w", err)
			}
		}
	}
	cust, err := p.api.customerByExternalID(ctx, org.Code)
	if err != nil {
		return fmt.Errorf("lookup customer: %w", err)
	}
	if cust != nil {
		p.logf("archive customer %s (%s)", cust.ID, cust.Name)
		p.stats.customersArchived++
		if p.write() {
			if err := p.api.deleteCustomer(ctx, cust.ID); err != nil {
				return fmt.Errorf("archive customer: %w", err)
			}
		}
	}
	return nil
}

type provisioner struct {
	api        writer
	book       *rateBook
	opt        options
	features   map[string]featureRecord
	firstAsset map[string]time.Time
	stats      struct {
		customersCreated, plansCreated, pricesCreated, entitlementsCreated, subscriptionsCreated, skipped int
		pricesReplaced, creditNotes                                                                       int
		plansArchived, customersArchived, plansRenamed, subscriptionsCancelled, lineItemsSynced           int
	}
}

func (p *provisioner) write() bool { return p.opt.apply }

func (p *provisioner) logf(format string, args ...any) {
	prefix := "[dry-run] "
	if p.write() {
		prefix = ""
	}
	fmt.Printf(prefix+format+"\n", args...)
}

// ensureCatalog creates the shared features and meters once per tenant.
func (p *provisioner) ensureCatalog(ctx context.Context) error {
	wanted := catalogFeatures()
	keys := make([]string, 0, len(wanted))
	for _, f := range wanted {
		keys = append(keys, f.LookupKey)
	}
	existing, err := p.api.featuresByLookupKeys(ctx, keys)
	if err != nil {
		return fmt.Errorf("list features: %w", err)
	}
	p.features = existing
	for _, f := range wanted {
		if _, ok := existing[f.LookupKey]; ok {
			continue
		}
		p.logf("create feature %s (%s)", f.LookupKey, f.Name)
		if !p.write() {
			p.features[f.LookupKey] = featureRecord{entity: entity{ID: "dry_" + f.LookupKey, LookupKey: f.LookupKey}}
			continue
		}
		created, err := p.api.createFeature(ctx, f.createRequest())
		if err != nil {
			return fmt.Errorf("create feature %s: %w", f.LookupKey, err)
		}
		p.features[f.LookupKey] = *created
	}
	return nil
}

func (p *provisioner) provisionOrg(ctx context.Context, org organization) error {
	card, cardKey := p.book.resolve(org)
	fmt.Printf("\n== %s  code=%s  rate_card=%s\n", org.Name, org.Code, cardKey)

	cust, err := p.ensureCustomer(ctx, org)
	if err != nil {
		return err
	}
	plan, err := p.ensurePlan(ctx, org, card, cardKey)
	if err != nil {
		return err
	}
	if !p.opt.subscribe {
		return nil
	}
	return p.ensureSubscription(ctx, org, cust, plan, card)
}

func (p *provisioner) ensureCustomer(ctx context.Context, org organization) (*customerRecord, error) {
	existing, err := p.api.customerByExternalID(ctx, org.Code)
	if err != nil {
		return nil, fmt.Errorf("lookup customer: %w", err)
	}
	if existing != nil {
		p.logf("customer exists %s (%s)", existing.ID, existing.Name)
		p.stats.skipped++
		patch := map[string]any{}
		if existing.Timezone != p.opt.timezone {
			p.logf("  set customer timezone %q -> %q", existing.Timezone, p.opt.timezone)
			patch["timezone"] = p.opt.timezone
		}
		if email := p.customerEmail(org); email != "" && existing.Email == "" {
			p.logf("  set customer email %s", email)
			patch["email"] = email
		}
		if len(patch) > 0 && p.write() {
			if err := p.api.updateCustomer(ctx, existing.ID, patch); err != nil {
				return nil, fmt.Errorf("update customer: %w", err)
			}
		}
		return existing, nil
	}
	req := map[string]any{
		"external_id":      org.Code,
		"name":             org.Name,
		"billing_currency": p.book.Currency,
		"timezone":         p.opt.timezone,
		"metadata": map[string]string{
			"source":           "evision",
			"evision_org_code": org.Code,
			"evision_org_type": org.Type,
		},
	}
	if org.Email != "" {
		req["email"] = org.Email
	} else if email := p.customerEmail(org); email != "" {
		req["email"] = email
	}
	p.logf("create customer external_id=%s name=%q", org.Code, org.Name)
	p.stats.customersCreated++
	if !p.write() {
		return &customerRecord{entity: entity{ID: "dry_customer"}, ExternalID: org.Code}, nil
	}
	return p.api.createCustomer(ctx, req)
}

// products an organisation may have been provisioned under before; a plan
// found under another product's key is renamed rather than duplicated.
var knownProducts = []string{"FleetForce", "TRUST"}

// customerEmail expands -email-template for the organisation.
func (p *provisioner) customerEmail(org organization) string {
	if p.opt.emailTmpl == "" {
		return ""
	}
	return strings.ReplaceAll(p.opt.emailTmpl, "{code}", strings.ToLower(org.Code))
}

func (p *provisioner) ensurePlan(ctx context.Context, org organization, card rateCard, cardKey string) (*planRecord, error) {
	key := org.planLookupKey(card.Product)
	name := card.Product + " – " + org.Name
	metadata := map[string]string{
		"evision_org_code": org.Code,
		"product":          card.Product,
		"rate_card":        cardKey,
	}
	plan, err := p.api.planByLookupKey(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("lookup plan: %w", err)
	}
	if plan == nil {
		plan, err = p.renameLegacyPlan(ctx, org, card.Product, key, name, metadata)
		if err != nil {
			return nil, err
		}
	}
	if plan == nil {
		req := map[string]any{
			"name":        name,
			"lookup_key":  key,
			"description": "Per-asset monthly billing, challan fetch overage and Google Maps usage for " + org.Name,
			"metadata":    metadata,
		}
		p.logf("create plan %s (%s)", key, name)
		p.stats.plansCreated++
		if p.write() {
			plan, err = p.api.createPlan(ctx, req)
			if err != nil {
				return nil, fmt.Errorf("create plan: %w", err)
			}
		} else {
			plan = &planRecord{entity: entity{ID: "dry_plan_" + key, LookupKey: key, Name: name}}
		}
	} else {
		p.logf("plan exists %s (%s)", plan.ID, plan.Name)
	}

	start, err := p.subscriptionStart(org)
	if err != nil {
		return nil, err
	}
	// Prices are matched by their lookup-key suffix so a renamed plan keeps its
	// prices. Every published price counts, ended ones included, so a price
	// that already ended is not created again. A live price that no longer
	// matches the card (amount or dates) or that the card no longer lists is
	// ended at the subscription start, or at its own start if later (never
	// billed); an exact duplicate is archived; the card's price is created
	// when none matches.
	all, err := p.api.planPrices(ctx, plan.ID)
	if err != nil {
		return nil, fmt.Errorf("list plan prices: %w", err)
	}
	existing := map[string][]priceRecord{}
	// Oldest first (ids are time-ordered), so a duplicate archive keeps the
	// original that subscriptions may already reference.
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	for _, pr := range all {
		if i := strings.Index(pr.LookupKey, "_"+sanitizeKey(org.Code)+"_"); i >= 0 {
			suffix := pr.LookupKey[i+len(sanitizeKey(org.Code))+2:]
			existing[suffix] = append(existing[suffix], pr)
		}
	}
	retire := func(pr priceRecord, why string) error {
		end := start
		if pr.StartDate != nil && pr.StartDate.After(end) {
			end = *pr.StartDate
		}
		p.logf("  retire price %s (%s %s): %s; ends %s", pr.LookupKey, pr.Amount, p.book.Currency, why, end.Format("2006-01-02"))
		p.stats.pricesReplaced++
		if !p.write() {
			return nil
		}
		if err := p.api.terminatePrice(ctx, pr.ID, end); err != nil {
			return fmt.Errorf("terminate price %s: %w", pr.LookupKey, err)
		}
		return nil
	}
	desired, err := buildPlanPrices(plan.ID, key, p.book.Currency, card, start)
	if err != nil {
		return nil, err
	}
	added := 0
	for _, pp := range desired {
		suffix := strings.TrimPrefix(pp.LookupKey, key+"_")
		kept := false
		var rest []priceRecord
		for _, pr := range existing[suffix] {
			switch {
			case !pp.same(pr):
				rest = append(rest, pr)
			case !kept:
				kept = true
			default:
				p.logf("  archive duplicate price %s %s", pr.ID, pr.LookupKey)
				if admin, ok := p.api.(dbAdmin); ok && p.write() {
					if err := admin.archivePrice(ctx, pr.ID); err != nil {
						return nil, err
					}
				}
			}
		}
		existing[suffix] = rest
		if kept {
			continue
		}
		for _, pr := range rest {
			if pr.EndDate == nil {
				if err := retire(pr, "differs from the rate card"); err != nil {
					return nil, err
				}
			}
		}
		delete(existing, suffix)
		added++
		feature, ok := p.features[pp.FeatureKey]
		if !ok || feature.MeterID == "" && p.write() {
			return nil, fmt.Errorf("feature %s has no meter", pp.FeatureKey)
		}
		pp.Request["meter_id"] = feature.MeterID
		p.logf("  price %-40s %6s %s %s %s", pp.DisplayName, pp.Request["amount"], p.book.Currency, describeModel(pp.Request), describeWindow(pp, start.Location()))
		p.stats.pricesCreated++
		if !p.write() {
			continue
		}
		if _, err := p.api.createPrice(ctx, pp.Request); err != nil {
			return nil, fmt.Errorf("create price %s: %w", pp.LookupKey, err)
		}
	}
	for _, suffix := range sortedKeys(existing) {
		for _, pr := range existing[suffix] {
			if pr.EndDate == nil {
				if err := retire(pr, "not on the rate card"); err != nil {
					return nil, err
				}
			}
		}
	}

	if added > 0 && len(all) > 0 && !p.opt.resub {
		p.logf("  sync %d new price(s) to active subscriptions", added)
		if p.write() {
			created, err := p.api.syncPlanPrices(ctx, plan.ID)
			if err != nil {
				return nil, fmt.Errorf("sync plan prices: %w", err)
			}
			p.stats.lineItemsSynced += created
			p.logf("  subscription line items created: %d", created)
		}
	}

	osm := p.features[featureOSMMaps]
	hasOSM := false
	for _, e := range plan.Entitlements {
		if e.FeatureID == osm.ID {
			hasOSM = true
		}
	}
	if !hasOSM {
		p.logf("  entitlement Open Source Maps: enabled (free)")
		p.stats.entitlementsCreated++
		if p.write() {
			err := p.api.createEntitlement(ctx, map[string]any{
				"plan_id":      plan.ID,
				"entity_type":  "PLAN",
				"entity_id":    plan.ID,
				"feature_id":   osm.ID,
				"feature_type": "boolean",
				"is_enabled":   true,
			})
			if err != nil {
				return nil, fmt.Errorf("create OSM entitlement: %w", err)
			}
		}
	}
	return plan, nil
}

func (p *provisioner) renameLegacyPlan(ctx context.Context, org organization, product, key, name string, metadata map[string]string) (*planRecord, error) {
	for _, other := range knownProducts {
		if other == product {
			continue
		}
		legacy, err := p.api.planByLookupKey(ctx, org.planLookupKey(other))
		if err != nil {
			return nil, fmt.Errorf("lookup plan: %w", err)
		}
		if legacy == nil {
			continue
		}
		p.logf("rename plan %s %q -> %q (%s)", legacy.ID, legacy.Name, name, key)
		p.stats.plansRenamed++
		if !p.write() {
			legacy.Name, legacy.LookupKey = name, key
			return legacy, nil
		}
		req := map[string]any{"name": name, "lookup_key": key, "metadata": metadata}
		if err := p.api.updatePlan(ctx, legacy.ID, req); err != nil {
			return nil, fmt.Errorf("rename plan: %w", err)
		}
		return p.api.planByLookupKey(ctx, key)
	}
	return nil, nil
}

func describeWindow(pp planPrice, loc *time.Location) string {
	if pp.End == nil {
		return "from " + pp.Start.In(loc).Format("2006-01-02")
	}
	return fmt.Sprintf("%s..%s", pp.Start.In(loc).Format("2006-01-02"), pp.End.In(loc).Format("2006-01-02"))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func describeModel(req map[string]any) string {
	if req["billing_model"] == "PACKAGE" {
		tq := req["transform_quantity"].(map[string]any)
		return fmt.Sprintf("per %v units", tq["divide_by"])
	}
	return "per unit"
}

// datedLineItems returns the plan prices that ended before today but cover
// part of a subscription starting at start. Subscription creation leaves
// expired prices out, so they are attached as explicit dated line items.
func (p *provisioner) datedLineItems(ctx context.Context, plan *planRecord, key string, card rateCard, start time.Time) ([]map[string]any, error) {
	desired, err := buildPlanPrices(plan.ID, key, p.book.Currency, card, start)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var ended []planPrice
	for _, pp := range desired {
		if pp.End != nil && !pp.End.After(now) && pp.End.After(pp.Start) {
			ended = append(ended, pp)
		}
	}
	if len(ended) == 0 {
		return nil, nil
	}
	var all []priceRecord
	if p.write() {
		if all, err = p.api.planPrices(ctx, plan.ID); err != nil {
			return nil, fmt.Errorf("lookup plan prices: %w", err)
		}
	}
	items := make([]map[string]any, 0, len(ended))
	for _, pp := range ended {
		item := map[string]any{"start_date": pp.Start, "end_date": *pp.End}
		if p.write() {
			pr, ok := lo.Find(all, func(pr priceRecord) bool { return pr.LookupKey == pp.LookupKey && pp.same(pr) })
			if !ok {
				return nil, fmt.Errorf("plan has no price %s ending %s", pp.LookupKey, pp.End.Format("2006-01-02"))
			}
			item["price_id"] = pr.ID
		}
		items = append(items, item)
	}
	return items, nil
}

func (p *provisioner) ensureSubscription(ctx context.Context, org organization, cust *customerRecord, plan *planRecord, card rateCard) error {
	subs, err := p.api.subscriptionsForCustomer(ctx, org.Code)
	if err != nil {
		return fmt.Errorf("list subscriptions: %w", err)
	}
	for _, s := range subs {
		if s.SubscriptionStatus != "active" && s.SubscriptionStatus != "trialing" {
			continue
		}
		if !p.opt.resub {
			p.logf("subscription exists %s (timezone %s)", s.ID, s.Timezone)
			return nil
		}
		p.logf("cancel subscription %s (timezone %s)", s.ID, s.Timezone)
		p.stats.subscriptionsCancelled++
		if p.write() {
			err := p.api.cancelSubscription(ctx, s.ID, map[string]any{
				"cancellation_type":                 "immediate",
				"cancel_immediately_invoice_policy": "skip",
			})
			if err != nil {
				return fmt.Errorf("cancel subscription %s: %w", s.ID, err)
			}
		}
	}
	start, err := p.subscriptionStart(org)
	if err != nil {
		return err
	}
	req := map[string]any{
		"customer_id":          cust.ID,
		"external_customer_id": org.Code,
		"plan_id":              plan.ID,
		"currency":             p.book.Currency,
		"billing_period":       "MONTHLY",
		"billing_period_count": 1,
		"billing_cycle":        "calendar",
		"start_date":           start.Format(time.RFC3339),
		"timezone":             p.opt.timezone,
		"metadata":             map[string]string{"evision_org_code": org.Code},
	}
	dated, err := p.datedLineItems(ctx, plan, org.planLookupKey(card.Product), card, start)
	if err != nil {
		return err
	}
	if len(dated) > 0 {
		req["line_items"] = dated
	}
	p.logf("create subscription plan=%s start=%s (%d dated line items for ended prices)", plan.LookupKey, start.Format("2006-01-02"), len(dated))
	p.stats.subscriptionsCreated++
	if !p.write() {
		return nil
	}
	_, err = p.api.createSubscription(ctx, req)
	return err
}

// subscriptionStart is -start-date, else local midnight of the day the org's
// first billable asset was onboarded, else the first day of the current month;
// never before the rate card's effective date.
func (p *provisioner) subscriptionStart(org organization) (time.Time, error) {
	loc, err := time.LoadLocation(p.opt.timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("timezone: %w", err)
	}
	if p.opt.startDate != "" {
		return time.ParseInLocation("2006-01-02", p.opt.startDate, loc)
	}
	var start time.Time
	if first, ok := p.firstAsset[org.Code]; ok {
		d := first.In(loc)
		start = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	} else {
		now := time.Now().In(loc)
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	}
	card, _ := p.book.resolve(org)
	periods, err := card.periods(loc)
	if err != nil {
		return time.Time{}, err
	}
	if periods[0].From.After(start) {
		start = periods[0].From
	}
	return start, nil
}

func (p *provisioner) summary() {
	mode := "would create"
	if p.write() {
		mode = "created"
	}
	if p.opt.rollback {
		verb := "would archive"
		if p.write() {
			verb = "archived"
		}
		fmt.Printf("\n%s: %d plans, %d customers\n", verb, p.stats.plansArchived, p.stats.customersArchived)
		return
	}
	fmt.Printf("\n%s: %d customers, %d plans, %d prices, %d entitlements, %d subscriptions (%d existing customers reused, %d plans renamed, %d prices replaced, %d subscriptions cancelled, %d line items synced)\n",
		mode, p.stats.customersCreated, p.stats.plansCreated, p.stats.pricesCreated,
		p.stats.entitlementsCreated, p.stats.subscriptionsCreated, p.stats.skipped, p.stats.plansRenamed, p.stats.pricesReplaced, p.stats.subscriptionsCancelled, p.stats.lineItemsSynced)
}
