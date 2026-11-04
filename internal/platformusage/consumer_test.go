package platformusage

import (
	"context"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/config"
	"github.com/flexprice/flexprice/internal/ee/service"
	"github.com/flexprice/flexprice/internal/logger"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingEvents struct {
	service.EventService
	ctx context.Context
	req *dto.IngestEventRequest
}

func (r *recordingEvents) CreateEvent(ctx context.Context, req *dto.IngestEventRequest) error {
	r.ctx, r.req = ctx, req
	return nil
}

const valid = `{"version":1,"id":"evt_asset_taporsi8drj_tapas1_2026-11-03","name":"asset_billing_state",
"organization_code":"TAPORSI8DRJ","occurred_at":"2026-11-03T00:00:00+05:30","source":"data-insights",
"properties":{"asset_code":"TAP-AS1","state":"ACTIVE","fetch_number_this_month":2}}`

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr string
	}{
		{name: "valid", payload: valid},
		{name: "not json", payload: `{`, wantErr: "JSON input"},
		{name: "wrong version", payload: `{"version":2,"id":"a","name":"n","organization_code":"o","occurred_at":"2026-11-03T00:00:00Z"}`, wantErr: "unsupported usage event version 2"},
		{name: "no id", payload: `{"version":1,"name":"n","organization_code":"o","occurred_at":"2026-11-03T00:00:00Z"}`, wantErr: "no id"},
		{name: "no name", payload: `{"version":1,"id":"a","organization_code":"o","occurred_at":"2026-11-03T00:00:00Z"}`, wantErr: "no name"},
		{name: "no organisation", payload: `{"version":1,"id":"a","name":"n","occurred_at":"2026-11-03T00:00:00Z"}`, wantErr: "no organization_code"},
		{name: "no time", payload: `{"version":1,"id":"a","name":"n","organization_code":"o"}`, wantErr: "no occurred_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.payload))
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestProcessIngestsForConfiguredTenant(t *testing.T) {
	events := &recordingEvents{}
	cfg := &config.Configuration{PlatformUsage: config.PlatformUsageConfig{
		Enabled: true, TenantID: "tenant_1", EnvironmentID: "env_1",
	}}
	log, err := logger.NewLogger(&config.Configuration{})
	require.NoError(t, err)
	c := NewConsumer(events, types.PlatformUsagePubSub{}, cfg, log)

	require.NoError(t, c.process(context.Background(), message.NewMessage("m1", []byte(valid))))

	assert.Equal(t, "tenant_1", types.GetTenantID(events.ctx))
	assert.Equal(t, "env_1", types.GetEnvironmentID(events.ctx))
	req := events.req
	assert.Equal(t, "asset_billing_state", req.EventName)
	assert.Equal(t, "evt_asset_taporsi8drj_tapas1_2026-11-03", req.EventID)
	assert.Equal(t, "TAPORSI8DRJ", req.ExternalCustomerID)
	assert.Equal(t, "data-insights", req.Source)
	assert.Equal(t, time.Date(2026, 11, 2, 18, 30, 0, 0, time.UTC), req.Timestamp)
	assert.Equal(t, "ACTIVE", req.Properties["state"])
	assert.Equal(t, float64(2), req.Properties["fetch_number_this_month"])

	err = c.process(context.Background(), message.NewMessage("m2", []byte(`{"version":1}`)))
	require.Error(t, err)
}

func TestKafkaConfigOverride(t *testing.T) {
	own := &config.Configuration{Kafka: config.KafkaConfig{Brokers: []string{"own:9092"}, TLS: true, UseSASL: true, SASLUser: "u"}}
	assert.Same(t, own, KafkaConfig(own))

	own.PlatformUsage.Kafka = config.PlatformUsageKafkaConfig{Brokers: []string{"platform:9092"}, UseSASL: true, SASLUser: "p", SASLPassword: "s"}
	got := KafkaConfig(own)
	assert.Equal(t, []string{"platform:9092"}, got.Kafka.Brokers)
	assert.False(t, got.Kafka.TLS)
	assert.Equal(t, "p", got.Kafka.SASLUser)
	assert.Equal(t, []string{"own:9092"}, own.Kafka.Brokers, "the main configuration is untouched")
}
