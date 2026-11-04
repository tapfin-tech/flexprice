package platformusage

import (
	"context"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/flexprice/flexprice/internal/config"
	"github.com/flexprice/flexprice/internal/ee/service"
	"github.com/flexprice/flexprice/internal/logger"
	pubsubRouter "github.com/flexprice/flexprice/internal/pubsub/router"
	"github.com/flexprice/flexprice/internal/types"
)

const handlerName = "platform_usage_handler"

// Consumer turns platform usage events into ingested events of the configured tenant.
type Consumer struct {
	events service.EventService
	ps     types.PlatformUsagePubSub
	cfg    config.PlatformUsageConfig
	log    *logger.Logger
}

func NewConsumer(events service.EventService, ps types.PlatformUsagePubSub, cfg *config.Configuration, log *logger.Logger) *Consumer {
	return &Consumer{events: events, ps: ps, cfg: cfg.PlatformUsage, log: log}
}

func (c *Consumer) RegisterHandler(r *pubsubRouter.Router, cfg *config.Configuration) {
	ctx := context.Background()
	if !c.cfg.Enabled {
		c.log.Info(ctx, "platform usage handler disabled by configuration")
		return
	}
	if c.cfg.TenantID == "" || c.cfg.EnvironmentID == "" || c.ps.PubSub == nil {
		c.log.Error(ctx, "platform usage handler not registered: tenant_id, environment_id and a pubsub are required",
			"error", "incomplete platform_usage configuration", "tenant_id", c.cfg.TenantID, "environment_id", c.cfg.EnvironmentID)
		return
	}

	throttle := middleware.NewThrottle(c.cfg.RateLimit, time.Second)
	r.AddNoPublishHandler(handlerName, c.cfg.Topic, c.cfg.TopicDLQ, c.ps, c.process, throttle.Middleware)

	c.log.Info(ctx, "registered platform usage handler",
		"handler_name", handlerName, "topic", c.cfg.Topic, "consumer_group", c.cfg.ConsumerGroup,
		"tenant_id", c.cfg.TenantID, "environment_id", c.cfg.EnvironmentID, "rate_limit", c.cfg.RateLimit)
}

// process ingests one message; a malformed one fails validation and goes to the DLQ.
func (c *Consumer) process(ctx context.Context, msg *message.Message) error {
	event, err := Parse(msg.Payload)
	if err != nil {
		return err
	}
	ctx = types.SetTenantID(ctx, c.cfg.TenantID)
	ctx = types.SetEnvironmentID(ctx, c.cfg.EnvironmentID)
	return c.events.CreateEvent(ctx, event.IngestRequest())
}

// KafkaConfig is the main configuration with the Kafka cluster swapped for the
// platform's when platform_usage.kafka.brokers is set.
func KafkaConfig(cfg *config.Configuration) *config.Configuration {
	override := cfg.PlatformUsage.Kafka
	if len(override.Brokers) == 0 {
		return cfg
	}
	c := *cfg
	c.Kafka.Brokers = override.Brokers
	c.Kafka.TLS = override.TLS
	c.Kafka.TLSCACertFile = ""
	c.Kafka.TLSServerName = ""
	c.Kafka.UseSASL = override.UseSASL
	c.Kafka.SASLMechanism = override.SASLMechanism
	c.Kafka.SASLUser = override.SASLUser
	c.Kafka.SASLPassword = override.SASLPassword
	return &c
}
