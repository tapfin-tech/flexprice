package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type assetRate struct {
	Active      decimal.Decimal `json:"active"`
	Hibernation decimal.Decimal `json:"hibernation"`
}

// rateCard is one organisation's price list: per-asset monthly rates by form
// factor and state, challan fetch overage and Google Maps package pricing.
type rateCard struct {
	Label     string `json:"label"`
	Product   string `json:"product,omitempty"`
	MatchName string `json:"match_name,omitempty"`
	// EffectiveFrom (YYYY-MM-DD) is the first billable day; a subscription
	// never starts before it, so earlier usage is a free trial.
	EffectiveFrom string               `json:"effective_from,omitempty"`
	AssetMonthly  map[string]assetRate `json:"asset_monthly"`
	// Revisions replace the asset rates from their own effective dates on.
	Revisions                   []rateRevision  `json:"revisions,omitempty"`
	ChallanFetchPerUnit         decimal.Decimal `json:"challan_fetch_per_unit"`
	ChallanFreePerAssetPerMonth int             `json:"challan_free_per_asset_per_month"`
	GoogleMapHitsPackageAmount  decimal.Decimal `json:"google_map_hits_package_amount"`
	GoogleMapHitsPackageSize    int             `json:"google_map_hits_package_size"`
	TaraAIPackageAmount         decimal.Decimal `json:"tara_ai_package_amount"`
	TaraAIPackageSize           int             `json:"tara_ai_package_size"`
}

// rateRevision is a later set of asset rates on the same card.
type rateRevision struct {
	EffectiveFrom string               `json:"effective_from"`
	AssetMonthly  map[string]assetRate `json:"asset_monthly"`
}

type rateBook struct {
	Currency string   `json:"currency"`
	Default  rateCard `json:"default"`
	// ThreeWheelerL5Models are model-name fragments (case-insensitive) that
	// mark a three-wheeler as category L5; every other three-wheeler is L3.
	ThreeWheelerL5Models []string            `json:"three_wheeler_l5_models,omitempty"`
	Overrides            map[string]rateCard `json:"overrides"`
}

// ratePeriod is one stretch of asset rates: from its start until the next
// period starts, or open-ended.
type ratePeriod struct {
	From  time.Time
	To    *time.Time
	Rates map[string]assetRate
}

// periods lays the card's base rates and revisions out as consecutive
// periods; the base starts at EffectiveFrom, or at the beginning of time.
func (c rateCard) periods(loc *time.Location) ([]ratePeriod, error) {
	out := []ratePeriod{{Rates: c.AssetMonthly}}
	if c.EffectiveFrom != "" {
		t, err := time.ParseInLocation("2006-01-02", c.EffectiveFrom, loc)
		if err != nil {
			return nil, fmt.Errorf("effective_from: %w", err)
		}
		out[0].From = t
	}
	for _, r := range c.Revisions {
		t, err := time.ParseInLocation("2006-01-02", r.EffectiveFrom, loc)
		if err != nil {
			return nil, fmt.Errorf("revision effective_from: %w", err)
		}
		if !t.After(out[len(out)-1].From) {
			return nil, fmt.Errorf("revision %s must start after the previous rates", r.EffectiveFrom)
		}
		out[len(out)-1].To = &t
		out = append(out, ratePeriod{From: t, Rates: r.AssetMonthly})
	}
	return out, nil
}

// threeWheelerCategory classifies a three-wheeler by model: L5 when the model
// matches one of the configured fragments, else L3.
func (b *rateBook) threeWheelerCategory(model string) string {
	m := strings.ToLower(model)
	for _, frag := range b.ThreeWheelerL5Models {
		if frag != "" && strings.Contains(m, strings.ToLower(frag)) {
			return "L5"
		}
	}
	return "L3"
}

func loadRateBook(path string) (*rateBook, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var book rateBook
	if err := json.Unmarshal(raw, &book); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if book.Currency == "" {
		book.Currency = "INR"
	}
	if book.Default.Product == "" {
		book.Default.Product = "FleetForce"
	}
	for key, card := range book.Overrides {
		if card.Product == "" {
			card.Product = book.Default.Product
			book.Overrides[key] = card
		}
	}
	if err := book.Default.validate(); err != nil {
		return nil, fmt.Errorf("default rate card: %w", err)
	}
	for key, card := range book.Overrides {
		if err := card.validate(); err != nil {
			return nil, fmt.Errorf("override %q: %w", key, err)
		}
	}
	return &book, nil
}

func (c rateCard) validate() error {
	if len(c.AssetMonthly) == 0 {
		return fmt.Errorf("asset_monthly is empty")
	}
	rates := []map[string]assetRate{c.AssetMonthly}
	for _, r := range c.Revisions {
		rates = append(rates, r.AssetMonthly)
	}
	for _, m := range rates {
		for ff := range m {
			if _, ok := formFactorLabel[ff]; !ok {
				return fmt.Errorf("asset_monthly has unknown form factor %q", ff)
			}
		}
	}
	if _, err := c.periods(time.UTC); err != nil {
		return err
	}
	if c.GoogleMapHitsPackageSize <= 0 {
		return fmt.Errorf("google_map_hits_package_size must be > 0")
	}
	if c.TaraAIPackageSize <= 0 {
		return fmt.Errorf("tara_ai_package_size must be > 0")
	}
	return nil
}

// resolve picks the override whose key equals the org code or whose match_name
// is contained in the org name (case-insensitive); otherwise the default card.
func (b *rateBook) resolve(org organization) (rateCard, string) {
	code := strings.ToLower(org.Code)
	name := strings.ToLower(org.Name)
	for key, card := range b.Overrides {
		if strings.ToLower(key) == code {
			return card, key
		}
		if card.MatchName != "" && strings.Contains(name, strings.ToLower(card.MatchName)) {
			return card, key
		}
	}
	return b.Default, "default"
}
