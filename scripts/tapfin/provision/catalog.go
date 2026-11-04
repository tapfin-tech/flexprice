package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Event names the FleetForce emitter must send to Flexprice.
const (
	eventAssetState   = "asset_billing_state"
	eventChallanFetch = "challan_fetch"
	eventMapHit       = "map_hit"
	eventTaraAI       = "tara_ai_usage"
)

// formFactors are every form factor the catalog meters; a plan prices only
// the ones its rate card lists.
var formFactors = []string{"TWO_WHEELER", "THREE_WHEELER", "THREE_WHEELER_L3", "THREE_WHEELER_L5", "FOUR_WHEELER", "TRUCK", "BUS", "BATTERY"}

var formFactorLabel = map[string]string{
	"TWO_WHEELER":      "2W",
	"THREE_WHEELER":    "3W",
	"THREE_WHEELER_L3": "3W L3",
	"THREE_WHEELER_L5": "3W L5",
	"FOUR_WHEELER":     "4W",
	"TRUCK":            "Truck",
	"BUS":              "Bus",
	"BATTERY":          "Battery",
}

// formFactorCategory splits a form factor whose meter also filters on the
// event's category property (three-wheeler L3/L5).
var formFactorCategory = map[string]struct{ formFactor, category string }{
	"THREE_WHEELER_L3": {"THREE_WHEELER", "L3"},
	"THREE_WHEELER_L5": {"THREE_WHEELER", "L5"},
}

// formFactorValues are the event property values a form factor's meter
// accepts; batteries also match evision's per-chemistry enum values.
var formFactorValues = map[string][]string{
	"BATTERY": {"BATTERY", "BATTERY - LFP", "BATTERY - NMC", "BATTERY - NCA", "BATTERY - LCO", "BATTERY - LTO", "BATTERY - LMO"},
}

func formFactorFilterValues(ff string) []string {
	if v, ok := formFactorValues[ff]; ok {
		return v
	}
	return []string{ff}
}

// formFactorFilters are the meter filters that select one form factor's
// asset events.
func formFactorFilters(ff string) []map[string]any {
	if c, ok := formFactorCategory[ff]; ok {
		return []map[string]any{
			{"key": "form_factor", "values": formFactorFilterValues(c.formFactor)},
			{"key": "category", "values": []string{c.category}},
		}
	}
	return []map[string]any{{"key": "form_factor", "values": formFactorFilterValues(ff)}}
}

var assetStates = []string{"active", "hibernation"}

// catalogFeature is a tenant-wide metered or boolean feature shared by every
// organisation's plan. Metered features own their meter.
type catalogFeature struct {
	LookupKey   string
	Name        string
	Type        string
	EventName   string
	Aggregation map[string]any
	Filters     []map[string]any
	UnitSingle  string
	UnitPlural  string
}

func assetFeatureKey(formFactor, state string) string {
	label := strings.ReplaceAll(strings.ToLower(formFactorLabel[formFactor]), " ", "_")
	return fmt.Sprintf("asset_%s_%s", label, state)
}

const (
	featureChallanOverage = "challan_fetch_billable"
	featureGoogleMapHits  = "map_hits_google"
	featureOSMMaps        = "map_hits_osm"
	featureTaraAITokens   = "tara_ai_tokens"
)

func catalogFeatures() []catalogFeature {
	var out []catalogFeature
	for _, ff := range formFactors {
		for _, state := range assetStates {
			out = append(out, catalogFeature{
				LookupKey: assetFeatureKey(ff, state),
				Name:      fmt.Sprintf("%s assets (%s)", formFactorLabel[ff], state),
				Type:      "metered",
				EventName: eventAssetState,
				Aggregation: map[string]any{
					"type":  "COUNT_UNIQUE",
					"field": "asset_code",
				},
				Filters: append(formFactorFilters(ff),
					map[string]any{"key": "state", "values": []string{strings.ToUpper(state)}}),
				UnitSingle: "asset",
				UnitPlural: "assets",
			})
		}
	}
	out = append(out,
		catalogFeature{
			LookupKey:   featureChallanOverage,
			Name:        "Challan fetches (billable)",
			Type:        "metered",
			EventName:   eventChallanFetch,
			Aggregation: map[string]any{"type": "COUNT"},
			Filters:     []map[string]any{{"key": "billable", "values": []string{"true"}}},
			UnitSingle:  "fetch",
			UnitPlural:  "fetches",
		},
		catalogFeature{
			LookupKey:   featureGoogleMapHits,
			Name:        "Google Maps hits",
			Type:        "metered",
			EventName:   eventMapHit,
			Aggregation: map[string]any{"type": "COUNT"},
			Filters:     []map[string]any{{"key": "provider", "values": []string{"google"}}},
			UnitSingle:  "hit",
			UnitPlural:  "hits",
		},
		catalogFeature{
			LookupKey: featureOSMMaps,
			Name:      "Open Source Maps",
			Type:      "boolean",
		},
		catalogFeature{
			LookupKey:   featureTaraAITokens,
			Name:        "TARA AI tokens",
			Type:        "metered",
			EventName:   eventTaraAI,
			Aggregation: map[string]any{"type": "SUM", "field": "tokens"},
			UnitSingle:  "token",
			UnitPlural:  "tokens",
		},
	)
	return out
}

func (c catalogFeature) createRequest() map[string]any {
	req := map[string]any{
		"name":       c.Name,
		"lookup_key": c.LookupKey,
		"type":       c.Type,
	}
	if c.Type != "metered" {
		return req
	}
	req["unit_singular"] = c.UnitSingle
	req["unit_plural"] = c.UnitPlural
	meter := map[string]any{
		"name":        c.Name,
		"event_name":  c.EventName,
		"aggregation": c.Aggregation,
		"reset_usage": "BILLING_PERIOD",
	}
	if len(c.Filters) > 0 {
		meter["filters"] = c.Filters
	}
	req["meter"] = meter
	return req
}

// planPrice is one price line of an organisation's plan, keyed by the feature
// whose meter it bills.
type planPrice struct {
	FeatureKey  string
	LookupKey   string
	DisplayName string
	Description string
	Amount      decimal.Decimal
	Start       time.Time
	End         *time.Time
	Request     map[string]any
}

// same reports whether an existing price can stand in for this one: same
// amount and end, and not starting later (a line item starts at the later of
// its subscription and its price, so an earlier price start changes nothing).
func (pp planPrice) same(pr priceRecord) bool {
	amount, err := decimal.NewFromString(pr.Amount)
	if err != nil || !amount.Equal(pp.Amount) {
		return false
	}
	if pr.StartDate != nil && pr.StartDate.After(pp.Start) {
		return false
	}
	return (pr.EndDate == nil) == (pp.End == nil) && (pp.End == nil || pr.EndDate.Equal(*pp.End))
}

// buildPlanPrices returns the card's prices for a plan, effective from start:
// a subscription line item starts at the later of its subscription and its
// price, and usage before the line item start is never billed. Asset rates
// with revisions become one dated price per rate period; a later period's
// lookup key carries its start date.
func buildPlanPrices(planID, planKey, currency string, card rateCard, start time.Time) ([]planPrice, error) {
	base := func(key, display, desc string, amount decimal.Decimal, from time.Time, to *time.Time) planPrice {
		pp := planPrice{
			FeatureKey:  key,
			LookupKey:   planKey + "_" + key,
			DisplayName: display,
			Description: desc,
			Amount:      amount,
			Start:       from.UTC(),
			Request: map[string]any{
				"amount":               amount.String(),
				"currency":             currency,
				"entity_type":          "PLAN",
				"entity_id":            planID,
				"type":                 "USAGE",
				"price_unit_type":      "FIAT",
				"billing_period":       "MONTHLY",
				"billing_period_count": 1,
				"billing_model":        "FLAT_FEE",
				"invoice_cadence":      "ARREAR",
				"lookup_key":           planKey + "_" + key,
				"display_name":         display,
				"description":          desc,
				"start_date":           from.UTC().Format(time.RFC3339),
			},
		}
		if to != nil {
			end := to.UTC()
			pp.End = &end
			pp.Request["end_date"] = end.Format(time.RFC3339)
		}
		return pp
	}

	periods, err := card.periods(start.Location())
	if err != nil {
		return nil, err
	}
	var prices []planPrice
	for i, period := range periods {
		from := period.From
		if start.After(from) {
			from = start
		}
		if period.To != nil && !period.To.After(from) {
			continue
		}
		suffix := ""
		if i > 0 {
			suffix = "_from" + period.From.Format("20060102")
		}
		for _, ff := range formFactors {
			rate, ok := period.Rates[ff]
			if !ok {
				continue
			}
			label := formFactorLabel[ff]
			active := base(assetFeatureKey(ff, "active"), label+" asset / month",
				fmt.Sprintf("%s asset in normal state, per asset per month", label), rate.Active, from, period.To)
			hib := base(assetFeatureKey(ff, "hibernation"), label+" asset (hibernation) / month",
				fmt.Sprintf("%s asset in hibernation state, per asset per month", label), rate.Hibernation, from, period.To)
			for _, pp := range []planPrice{active, hib} {
				pp.LookupKey += suffix
				pp.Request["lookup_key"] = pp.LookupKey
				prices = append(prices, pp)
			}
		}
	}
	base0 := func(key, display, desc string, amount decimal.Decimal) planPrice {
		return base(key, display, desc, amount, start, nil)
	}

	challan := base0(featureChallanOverage, "Challan fetch (beyond free quota)",
		fmt.Sprintf("%d free fetch per asset per month, then per fetch", card.ChallanFreePerAssetPerMonth),
		card.ChallanFetchPerUnit)
	prices = append(prices, challan)

	maps := base0(featureGoogleMapHits, fmt.Sprintf("Google Maps hits / %d", card.GoogleMapHitsPackageSize),
		fmt.Sprintf("Per %d Google Maps hits, rounded up", card.GoogleMapHitsPackageSize),
		card.GoogleMapHitsPackageAmount)
	maps.Request["billing_model"] = "PACKAGE"
	maps.Request["transform_quantity"] = map[string]any{
		"divide_by": card.GoogleMapHitsPackageSize,
		"round":     "up",
	}
	prices = append(prices, maps)

	tara := base0(featureTaraAITokens, fmt.Sprintf("TARA AI tokens / %s", humanCount(card.TaraAIPackageSize)),
		fmt.Sprintf("Per %s TARA AI tokens, rounded up", humanCount(card.TaraAIPackageSize)),
		card.TaraAIPackageAmount)
	tara.Request["billing_model"] = "PACKAGE"
	tara.Request["transform_quantity"] = map[string]any{
		"divide_by": card.TaraAIPackageSize,
		"round":     "up",
	}
	prices = append(prices, tara)
	return prices, nil
}

func humanCount(n int) string {
	switch {
	case n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n%1_000 == 0:
		return fmt.Sprintf("%dK", n/1_000)
	default:
		return fmt.Sprint(n)
	}
}
