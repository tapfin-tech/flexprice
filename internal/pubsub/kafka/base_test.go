package kafka

import (
	"testing"
	"time"

	"github.com/flexprice/flexprice/internal/config"
)

func TestGetSaramaConfigSetsOffsetRetention(t *testing.T) {
	const retention = 7 * 24 * time.Hour
	cfg, err := GetSaramaConfig(&config.Configuration{Kafka: config.KafkaConfig{OffsetRetention: retention}})
	if err != nil {
		t.Fatalf("GetSaramaConfig() error = %v, want nil", err)
	}

	if cfg.Consumer.Offsets.Retention != retention {
		t.Fatalf("consumer offset retention = %s, want %s", cfg.Consumer.Offsets.Retention, retention)
	}
}

func TestGetSaramaConfigSASLPlaintextSkipsTLS(t *testing.T) {
	cfg, err := GetSaramaConfig(&config.Configuration{Kafka: config.KafkaConfig{
		UseSASL: true, SASLPlaintext: true, SASLMechanism: "SCRAM-SHA-256", SASLUser: "u", SASLPassword: "p",
	}})
	if err != nil {
		t.Fatalf("GetSaramaConfig() error = %v", err)
	}
	if cfg.Net.TLS.Enable {
		t.Error("TLS.Enable = true, want false with sasl_plaintext")
	}
	if !cfg.Net.SASL.Enable {
		t.Error("SASL.Enable = false, want true")
	}
}
