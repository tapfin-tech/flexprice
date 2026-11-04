// Package platformusage ingests the usage events the tapfin platform publishes to
// Kafka, so the platform never speaks a billing vendor's API.
//
// Contract (usage_event/v1): one JSON message per occurrence, keyed by organisation code.
//
//	{
//	  "version": 1,
//	  "id": "evt_asset_taporsi8drj_tapas1_2026-11-03",   // stable per occurrence; duplicates are ignored
//	  "name": "asset_billing_state",                      // what happened; the meter's event name
//	  "organization_code": "TAPORSI8DRJ",                 // who is billed; the customer's external id
//	  "occurred_at": "2026-11-03T00:00:00+05:30",         // RFC 3339
//	  "source": "data-insights",                          // which system saw it
//	  "properties": {"asset_code": "TAP-AS1", "state": "ACTIVE"}
//	}
package platformusage

import (
	"encoding/json"
	"time"

	"github.com/flexprice/flexprice/internal/api/dto"
	ierr "github.com/flexprice/flexprice/internal/errors"
)

// Version is the only envelope version this consumer accepts.
const Version = 1

// Event is one usage_event/v1 message.
type Event struct {
	Version          int            `json:"version"`
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	OrganizationCode string         `json:"organization_code"`
	OccurredAt       time.Time      `json:"occurred_at"`
	Source           string         `json:"source"`
	Properties       map[string]any `json:"properties"`
}

// Parse decodes and validates one message payload.
func Parse(payload []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return e, ierr.WithError(err).WithHint("Invalid usage event").Mark(ierr.ErrValidation)
	}
	switch {
	case e.Version != Version:
		return e, ierr.NewErrorf("unsupported usage event version %d", e.Version).Mark(ierr.ErrValidation)
	case e.ID == "":
		return e, ierr.NewError("usage event has no id").Mark(ierr.ErrValidation)
	case e.Name == "":
		return e, ierr.NewError("usage event has no name").Mark(ierr.ErrValidation)
	case e.OrganizationCode == "":
		return e, ierr.NewError("usage event has no organization_code").Mark(ierr.ErrValidation)
	case e.OccurredAt.IsZero():
		return e, ierr.NewError("usage event has no occurred_at").Mark(ierr.ErrValidation)
	}
	return e, nil
}

// IngestRequest is the event as the ingest pipeline takes it.
func (e Event) IngestRequest() *dto.IngestEventRequest {
	props := e.Properties
	if props == nil {
		props = map[string]any{}
	}
	return &dto.IngestEventRequest{
		EventName:          e.Name,
		EventID:            e.ID,
		ExternalCustomerID: e.OrganizationCode,
		Timestamp:          e.OccurredAt.UTC(),
		Source:             e.Source,
		Properties:         props,
	}
}
