package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
)

func roundTrip(t *testing.T, in any, out interface{ Validate() error }) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal into %T: %v\n%s", out, err, raw)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("%T rejected: %v\n%s", out, err, raw)
	}
}

func TestCatalogFeaturesValidate(t *testing.T) {
	for _, f := range catalogFeatures() {
		roundTrip(t, f.createRequest(), &dto.CreateFeatureRequest{})
	}
}

func TestPlanPricesValidate(t *testing.T) {
	book, err := loadRateBook("ratecard.json")
	if err != nil {
		t.Fatal(err)
	}
	cards := map[string]rateCard{"default": book.Default}
	for k, c := range book.Overrides {
		cards[k] = c
	}
	for key, card := range cards {
		prices, err := buildPlanPrices("plan_test", "fleetforce_test", book.Currency, card, time.Date(2026, 2, 25, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		want := 2*len(card.AssetMonthly) + 3
		for _, r := range card.Revisions {
			want += 2 * len(r.AssetMonthly)
		}
		if len(prices) != want {
			t.Fatalf("%s: expected %d prices, got %d", key, want, len(prices))
		}
		seen := map[string]bool{}
		for _, p := range prices {
			if seen[p.LookupKey] {
				t.Fatalf("duplicate price lookup key %s", p.LookupKey)
			}
			seen[p.LookupKey] = true
			p.Request["meter_id"] = "meter_test"
			roundTrip(t, p.Request, &dto.CreatePriceRequest{})
		}
	}
}

func TestOtherRequestsValidate(t *testing.T) {
	roundTrip(t, map[string]any{
		"name": "FleetForce – Test", "lookup_key": "fleetforce_test",
		"metadata": map[string]string{"evision_org_code": "X"},
	}, &dto.CreatePlanRequest{})

	roundTrip(t, map[string]any{
		"external_id": "TAPORQ03UH5", "name": "Bansal Credits Limited", "billing_currency": "INR",
		"metadata": map[string]string{"source": "evision"},
	}, &dto.CreateCustomerRequest{})

	roundTrip(t, map[string]any{
		"plan_id": "plan_test", "entity_type": "PLAN", "entity_id": "plan_test",
		"feature_id": "feat_test", "feature_type": "boolean", "is_enabled": true,
	}, &dto.CreateEntitlementRequest{})

	book, err := loadRateBook("ratecard.json")
	if err != nil {
		t.Fatal(err)
	}
	p := &provisioner{opt: options{timezone: "Asia/Kolkata", startDate: "2026-12-01"}, book: book}
	start, err := p.subscriptionStart(organization{Code: "TAPORQ03UH5"})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, map[string]any{
		"customer_id": "cust_test", "external_customer_id": "TAPORQ03UH5", "plan_id": "plan_test",
		"currency": "INR", "billing_period": "MONTHLY", "billing_period_count": 1,
		"billing_cycle": "calendar", "start_date": start, "timezone": "Asia/Kolkata",
	}, &dto.CreateSubscriptionRequest{})
}

func TestRateBookResolve(t *testing.T) {
	book, err := loadRateBook("ratecard.json")
	if err != nil {
		t.Fatal(err)
	}
	_, key := book.resolve(organization{Code: "TAPORQ03UH5", Name: "Bansal Credits Limited"})
	if key != "TAPORQ03UH5" {
		t.Fatalf("Bansal Credits resolved to %s", key)
	}
	_, key = book.resolve(organization{Code: "TAPORHKBROU", Name: "HARIKISHAN BANSAL"})
	if key != "default" {
		t.Fatalf("driver surnamed Bansal resolved to %s, want default", key)
	}
	bansal := book.Overrides["TAPORQ03UH5"]
	if bansal.Product != "TRUST" || book.Default.Product != "FleetForce" {
		t.Fatalf("products: bansal=%q default=%q", bansal.Product, book.Default.Product)
	}
	org := organization{Code: "TAPORQ03UH5"}
	if got := org.planLookupKey(bansal.Product); got != "trust_taporq03uh5" {
		t.Fatalf("lookup key %q", got)
	}
	if _, ok := bansal.AssetMonthly["TRUCK"]; !ok {
		t.Fatal("Bansal card should price TRUCK")
	}
	amu := book.Overrides["TAPORSI8DRJ"]
	if bat := amu.AssetMonthly["BATTERY"]; bat.Active.String() != "65" || bat.Hibernation.String() != "10" {
		t.Fatalf("AMU battery rate %+v", bat)
	}
	if ff, chem := normalizeFormFactor("BATTERY - LFP", "BATTERY"); ff != "BATTERY" || chem != "LFP" {
		t.Fatalf("normalize battery: %q %q", ff, chem)
	}
	if ff, _ := normalizeFormFactor("TRUCK", "VEHICLE"); ff != "TRUCK" {
		t.Fatalf("normalize truck: %q", ff)
	}
}

func TestSubscriptionStartFromFirstAsset(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	// 2026-03-24 02:15 IST, stored as a UTC instant
	first := time.Date(2026, 3, 23, 20, 45, 0, 0, time.UTC)
	book, _ := loadRateBook("ratecard.json")
	p := &provisioner{opt: options{timezone: "Asia/Kolkata"}, firstAsset: map[string]time.Time{"ORG": first}, book: book}
	start, err := p.subscriptionStart(organization{Code: "ORG"})
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 3, 24, 0, 0, 0, 0, loc); !start.Equal(want) {
		t.Fatalf("start %v, want %v", start, want)
	}
	start, _ = p.subscriptionStart(organization{Code: "NO_ASSETS"})
	if start.Day() != 1 || start.Location().String() != loc.String() {
		t.Fatalf("org without assets should start on the first of the month, got %v", start)
	}
	p.opt.startDate = "2026-12-01"
	start, _ = p.subscriptionStart(organization{Code: "ORG"})
	if !start.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("-start-date should win, got %v", start)
	}
}

func TestAssetStateEvents(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	day := func(d string) time.Time {
		t, _ := time.ParseInLocation("2006-01-02", d, loc)
		return t
	}
	ptr := func(t time.Time) *time.Time { return &t }
	links := []assetLink{
		// live the whole window
		{AssetCode: "A", OrgCode: "ORG", FormFactor: "TWO_WHEELER", Onboarding: day("2026-10-01")},
		// onboarded mid-window, offboarded before the end
		{AssetCode: "B", OrgCode: "ORG", FormFactor: "THREE_WHEELER", Onboarding: day("2026-11-02"), Offboarding: ptr(day("2026-11-04"))},
		// hibernating on day 2 and 3 only
		{AssetCode: "C", OrgCode: "ORG", FormFactor: "TWO_WHEELER", Onboarding: day("2026-10-01"), HibStart: ptr(day("2026-11-02")), HibEnd: ptr(day("2026-11-04"))},
	}
	evts := assetStateEvents(links, day("2026-11-01"), day("2026-11-05"), loc, "t", "e")
	got := map[string]string{}
	for _, e := range evts {
		got[e.Properties["asset_code"].(string)+"@"+e.Properties["date"].(string)] = e.Properties["state"].(string)
	}
	want := map[string]string{
		"A@2026-11-01": "ACTIVE", "A@2026-11-02": "ACTIVE", "A@2026-11-03": "ACTIVE", "A@2026-11-04": "ACTIVE", "A@2026-11-05": "ACTIVE",
		"B@2026-11-02": "ACTIVE", "B@2026-11-03": "ACTIVE",
		"C@2026-11-01": "ACTIVE", "C@2026-11-02": "HIBERNATION", "C@2026-11-03": "HIBERNATION", "C@2026-11-04": "ACTIVE", "C@2026-11-05": "ACTIVE",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %q want %q", k, got[k], v)
		}
	}
	if ts := evts[0].Timestamp; !ts.Equal(day("2026-11-01")) || ts.Location() != time.UTC {
		t.Fatalf("timestamp %v should be local midnight stored in UTC", ts)
	}
	ids := map[string]bool{}
	for _, e := range evts {
		if ids[e.ID] {
			t.Fatalf("duplicate event id %s", e.ID)
		}
		ids[e.ID] = true
	}
}

func TestStubFraction(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	cases := []struct {
		start      string
		used, full int
	}{
		{"2026-02-25", 4, 28},
		{"2026-03-24", 8, 31},
		{"2026-05-29", 3, 31},
		{"2026-11-01", 30, 30},
	}
	for _, c := range cases {
		start, _ := time.ParseInLocation("2006-01-02", c.start, loc)
		fraction, used, full := stubFraction(start.UTC(), loc)
		if used != c.used || full != c.full {
			t.Fatalf("%s: got %d/%d, want %d/%d", c.start, used, full, c.used, c.full)
		}
		if c.used == c.full && !fraction.Equal(decimal.NewFromInt(1)) {
			t.Fatalf("%s: full month should be 1, got %s", c.start, fraction)
		}
	}
}

func TestPlanRedates(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	end := func(y int, m time.Month) *time.Time {
		t := time.Date(y, m, 1, 0, 0, 0, 0, ist).UTC()
		return &t
	}
	cfg := types.InvoiceConfig{
		InvoiceNumberPrefix: "INV", InvoiceNumberFormat: types.InvoiceNumberFormatYYYYMM, InvoiceNumberStartSequence: 1,
		InvoiceNumberTimezone: "Asia/Kolkata", InvoiceNumberSeparator: "-", InvoiceNumberSuffixLength: 5,
	}
	invs := []invoiceRecord{
		{ID: "c", InvoiceNumber: "INV-202611-00003", SubscriptionID: "s2", PeriodEnd: end(2026, time.November)},
		{ID: "b", InvoiceNumber: "INV-202611-00002", SubscriptionID: "s1", PeriodEnd: end(2026, time.November)},
		{ID: "a", InvoiceNumber: "INV-202611-00001", SubscriptionID: "s1", PeriodEnd: end(2026, time.March)},
	}
	plan, last, err := planRedates(invs, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"INV-202603-00001", "INV-202611-00001", "INV-202611-00002"}
	for i, r := range plan {
		if r.number != want[i] {
			t.Fatalf("plan[%d] = %s, want %s", i, r.number, want[i])
		}
		if !r.issue.Equal(*r.inv.PeriodEnd) {
			t.Fatalf("issue %v should equal period end", r.issue)
		}
	}
	if plan[1].inv.ID != "b" || plan[2].inv.ID != "c" {
		t.Fatal("same-day invoices should keep their existing order")
	}
	if last["202603"] != 1 || last["202611"] != 2 {
		t.Fatalf("sequences %v", last)
	}
	if _, _, err := planRedates([]invoiceRecord{{ID: "x"}}, cfg); err == nil {
		t.Fatal("an invoice without a period must be refused")
	}
}

func TestEffectiveDatedCards(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	book, err := loadRateBook("ratecard.json")
	if err != nil {
		t.Fatal(err)
	}
	day := func(d string) time.Time {
		t, _ := time.ParseInLocation("2006-01-02", d, ist)
		return t
	}
	// Bansal: first asset in March, card effective April 1.
	p := &provisioner{opt: options{timezone: "Asia/Kolkata"}, book: book,
		firstAsset: map[string]time.Time{"TAPORQ03UH5": day("2026-03-24").UTC(), "TAPORSI8DRJ": day("2026-05-29").UTC()}}
	start, _ := p.subscriptionStart(organization{Code: "TAPORQ03UH5"})
	if !start.Equal(day("2026-04-01")) {
		t.Fatalf("Bansal start %v", start)
	}
	start, _ = p.subscriptionStart(organization{Code: "TAPORSI8DRJ"})
	if !start.Equal(day("2026-07-15")) {
		t.Fatalf("AMU start %v", start)
	}

	amu := book.Overrides["TAPORSI8DRJ"]
	prices, err := buildPlanPrices("plan", "trust_taporsi8drj", "INR", amu, start)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]planPrice{}
	for _, pp := range prices {
		byKey[pp.LookupKey] = pp
	}
	l5 := byKey["trust_taporsi8drj_asset_3w_l5_active"]
	if !l5.Amount.Equal(decimal.NewFromInt(130)) || !l5.Start.Equal(day("2026-07-15")) || l5.End == nil || !l5.End.Equal(day("2026-11-01")) {
		t.Fatalf("first-period L5 price %+v", l5)
	}
	l5v2 := byKey["trust_taporsi8drj_asset_3w_l5_active_from20261101"]
	if !l5v2.Amount.Equal(decimal.NewFromInt(145)) || !l5v2.Start.Equal(day("2026-11-01")) || l5v2.End != nil {
		t.Fatalf("revised L5 price %+v", l5v2)
	}
	if _, ok := byKey["trust_taporsi8drj_asset_3w_active"]; ok {
		t.Fatal("AMU card must not price undivided 3W")
	}
	if byKey["trust_taporsi8drj_asset_bus_hibernation"].Amount.String() != "10" {
		t.Fatal("hibernation should be 10 for every form factor")
	}
	if !byKey["trust_taporsi8drj_challan_fetch_billable"].Start.Equal(day("2026-07-15")) {
		t.Fatal("non-asset prices start with the subscription")
	}

	// Subscribing after the revision keeps only the revised rates, under the dated key.
	late, _ := buildPlanPrices("plan", "trust_taporsi8drj", "INR", amu, day("2026-12-01"))
	for _, pp := range late {
		if pp.FeatureKey == "asset_3w_l3_active" && (pp.LookupKey != "trust_taporsi8drj_asset_3w_l3_active_from20261101" || !pp.Start.Equal(day("2026-12-01"))) {
			t.Fatalf("late L3 price %+v", pp)
		}
	}

	if book.threeWheelerCategory("HiLoad EV") != "L5" || book.threeWheelerCategory("6S") != "L3" || book.threeWheelerCategory("") != "L3" {
		t.Fatal("three-wheeler classification")
	}
	existing := priceRecord{Amount: "130", StartDate: lo.ToPtr(day("2026-07-15").UTC()), EndDate: lo.ToPtr(day("2026-11-01").UTC())}
	if !l5.same(existing) {
		t.Fatal("same price should be kept")
	}
	existing.Amount = "60"
	if l5.same(existing) {
		t.Fatal("changed amount must be detected")
	}
	earlier := priceRecord{Amount: "130", StartDate: lo.ToPtr(day("2026-05-29").UTC()), EndDate: lo.ToPtr(day("2026-11-01").UTC())}
	if !l5.same(earlier) {
		t.Fatal("an earlier-starting identical price should be kept")
	}
}
