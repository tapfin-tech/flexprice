package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/flexprice/flexprice/internal/domain/events"
)

// Onboarding statuses under which an asset is billable to the organisation.
const defaultBillableStatuses = "ONBOARDING_SUCCESS,ACTIVE,HIBERNATED,DEBOARDING_REQUESTED,DEBOARDED"

const assetEventSource = "evision-asset-sync"

// assetLink is one row of evision asset_x_organization joined to the asset.
type assetLink struct {
	AssetCode  string
	OrgCode    string
	FormFactor string
	AssetType  string
	Model      string
	// Category is the three-wheeler class (L3/L5); empty for other assets.
	Category    string
	Status      string
	Onboarding  time.Time
	Offboarding *time.Time
	HibStart    *time.Time
	HibEnd      *time.Time
}

func fetchAssetLinks(ctx context.Context, conn driver.Conn, db string, orgCodes, statuses []string) ([]assetLink, error) {
	query := `
		SELECT axo.asset_code, axo.organization_code, toString(a.form_factor), toString(a.asset_type),
		       JSONExtractString(a.additional_data, 'model'),
		       toString(axo.onboarding_status), axo.onboarding_datetime, axo.offboarding_datetime,
		       axo.hibernation_start, axo.hibernation_end
		FROM ` + db + `.asset_x_organization axo
		ANY LEFT JOIN ` + db + `.asset a ON a.code = axo.asset_code
		WHERE axo.organization_code IN ? AND toString(axo.onboarding_status) IN ?
		  AND axo.onboarding_datetime IS NOT NULL
		ORDER BY axo.organization_code, axo.asset_code, axo.onboarding_datetime`
	rows, err := conn.Query(ctx, query, orgCodes, statuses)
	if err != nil {
		return nil, fmt.Errorf("query asset links: %w", err)
	}
	defer rows.Close()

	var links []assetLink
	for rows.Next() {
		var l assetLink
		var on *int32
		var off, hs, he *int32
		if err := rows.Scan(&l.AssetCode, &l.OrgCode, &l.FormFactor, &l.AssetType, &l.Model, &l.Status, &on, &off, &hs, &he); err != nil {
			return nil, err
		}
		if on == nil {
			continue
		}
		l.Onboarding = epoch(*on)
		l.Offboarding = epochPtr(off)
		l.HibStart = epochPtr(hs)
		l.HibEnd = epochPtr(he)
		if l.Offboarding != nil && !l.Offboarding.After(l.Onboarding) {
			continue
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

func epoch(s int32) time.Time { return time.Unix(int64(s), 0).UTC() }

func epochPtr(s *int32) *time.Time {
	if s == nil {
		return nil
	}
	t := epoch(*s)
	return &t
}

// within reports whether the [start, end) window overlaps the day [dayStart, dayEnd).
func within(start time.Time, end *time.Time, dayStart, dayEnd time.Time) bool {
	if !start.Before(dayEnd) {
		return false
	}
	return end == nil || end.After(dayStart)
}

// assetStateEvents emits one asset_billing_state event per link per day the
// link is live, timestamped at local midnight of that day.
func assetStateEvents(links []assetLink, from, to time.Time, loc *time.Location, tenantID, envID string) []*events.Event {
	var out []*events.Event
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
		dayEnd := dayStart.AddDate(0, 0, 1)
		for _, l := range links {
			if !within(l.Onboarding, l.Offboarding, dayStart, dayEnd) {
				continue
			}
			state := "ACTIVE"
			if l.HibStart != nil && within(*l.HibStart, l.HibEnd, dayStart, dayEnd) {
				state = "HIBERNATION"
			}
			date := dayStart.Format("2006-01-02")
			formFactor, chemistry := normalizeFormFactor(l.FormFactor, l.AssetType)
			props := map[string]interface{}{
				"asset_code":  l.AssetCode,
				"asset_type":  l.AssetType,
				"form_factor": formFactor,
				"chemistry":   chemistry,
				"state":       state,
				"date":        date,
			}
			if l.Category != "" {
				props["category"] = l.Category
			}
			out = append(out, &events.Event{
				ID:                 fmt.Sprintf("evt_asset_%s_%s_%s", strings.ToLower(l.OrgCode), sanitizeKey(l.AssetCode), date),
				TenantID:           tenantID,
				EnvironmentID:      envID,
				ExternalCustomerID: l.OrgCode,
				EventName:          eventAssetState,
				Source:             assetEventSource,
				Timestamp:          dayStart.UTC(),
				Properties:         props,
			})
		}
	}
	return out
}

// classifyThreeWheelers sets the L3/L5 category on three-wheeler links.
func classifyThreeWheelers(links []assetLink, book *rateBook) {
	for i := range links {
		if links[i].FormFactor == "THREE_WHEELER" {
			links[i].Category = book.threeWheelerCategory(links[i].Model)
		}
	}
}

// normalizeFormFactor folds evision's per-chemistry battery form factors
// ("BATTERY - LFP") into BATTERY and returns the chemistry separately.
func normalizeFormFactor(formFactor, assetType string) (string, string) {
	if strings.HasPrefix(formFactor, "BATTERY") {
		chem := strings.TrimSpace(strings.TrimPrefix(formFactor, "BATTERY"))
		return "BATTERY", strings.TrimSpace(strings.TrimPrefix(chem, "-"))
	}
	if assetType == "BATTERY" {
		return "BATTERY", formFactor
	}
	return formFactor, ""
}

// assetSummary counts, per organisation and month, the distinct assets and
// asset-days seen in each form factor and state: what the meters will bill.
type assetSummary struct {
	Org, Month, FormFactor, State string
	Assets                        map[string]bool
	AssetDays                     int
}

func summarizeAssetEvents(evts []*events.Event, loc *time.Location) []*assetSummary {
	index := map[string]*assetSummary{}
	for _, e := range evts {
		month := e.Timestamp.In(loc).Format("2006-01")
		ff := fmt.Sprint(e.Properties["form_factor"])
		if c, ok := e.Properties["category"]; ok {
			ff += " " + fmt.Sprint(c)
		}
		state := fmt.Sprint(e.Properties["state"])
		key := strings.Join([]string{e.ExternalCustomerID, month, ff, state}, "|")
		s, ok := index[key]
		if !ok {
			s = &assetSummary{Org: e.ExternalCustomerID, Month: month, FormFactor: ff, State: state, Assets: map[string]bool{}}
			index[key] = s
		}
		s.Assets[fmt.Sprint(e.Properties["asset_code"])] = true
		s.AssetDays++
	}
	out := make([]*assetSummary, 0, len(index))
	for _, s := range index {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Org != b.Org {
			return a.Org < b.Org
		}
		if a.Month != b.Month {
			return a.Month < b.Month
		}
		if a.FormFactor != b.FormFactor {
			return a.FormFactor < b.FormFactor
		}
		return a.State < b.State
	})
	return out
}

// fetchFirstOnboarding returns, per organisation, the onboarding time of its
// earliest billable asset link.
func fetchFirstOnboarding(ctx context.Context, conn driver.Conn, db string, orgCodes, statuses []string) (map[string]time.Time, error) {
	query := `
		SELECT organization_code, min(onboarding_datetime)
		FROM ` + db + `.asset_x_organization
		WHERE organization_code IN ? AND toString(onboarding_status) IN ?
		  AND onboarding_datetime IS NOT NULL
		GROUP BY organization_code`
	rows, err := conn.Query(ctx, query, orgCodes, statuses)
	if err != nil {
		return nil, fmt.Errorf("query first onboarding: %w", err)
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var code string
		var first *int32
		if err := rows.Scan(&code, &first); err != nil {
			return nil, err
		}
		if first != nil {
			out[code] = epoch(*first)
		}
	}
	return out, rows.Err()
}
