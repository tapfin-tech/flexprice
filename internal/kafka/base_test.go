package kafka

import (
	"testing"
	"time"

	"github.com/Shopify/sarama"

	"github.com/flexprice/flexprice/internal/config"
)

func TestGetSaramaConfigSetsOffsetRetention(t *testing.T) {
	const retention = 7 * 24 * time.Hour
	cfg, err := GetSaramaConfig(&config.KafkaConfig{OffsetRetention: retention})
	if err != nil {
		t.Fatalf("GetSaramaConfig() error = %v, want nil", err)
	}

	if cfg.Consumer.Offsets.Retention != retention {
		t.Fatalf("consumer offset retention = %s, want %s", cfg.Consumer.Offsets.Retention, retention)
	}
}

func TestGetSaramaConfigSASLTransport(t *testing.T) {
	tests := []struct {
		name          string
		cfg           config.KafkaConfig
		wantTLS       bool
		wantSASL      bool
		wantMechanism sarama.SASLMechanism
	}{
		{name: "plain", cfg: config.KafkaConfig{}},
		{
			name:          "sasl defaults to tls",
			cfg:           config.KafkaConfig{UseSASL: true, SASLMechanism: sarama.SASLTypeSCRAMSHA256},
			wantTLS:       true,
			wantSASL:      true,
			wantMechanism: sarama.SASLTypeSCRAMSHA256,
		},
		{
			name:          "sasl plaintext skips tls",
			cfg:           config.KafkaConfig{UseSASL: true, SASLPlaintext: true, SASLMechanism: sarama.SASLTypeSCRAMSHA256, SASLUser: "u", SASLPassword: "p"},
			wantSASL:      true,
			wantMechanism: sarama.SASLTypeSCRAMSHA256,
		},
		{
			name:          "explicit tls wins over sasl plaintext",
			cfg:           config.KafkaConfig{TLS: true, UseSASL: true, SASLPlaintext: true, SASLMechanism: sarama.SASLTypeSCRAMSHA512},
			wantTLS:       true,
			wantSASL:      true,
			wantMechanism: sarama.SASLTypeSCRAMSHA512,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetSaramaConfig(&tt.cfg)
			if err != nil {
				t.Fatalf("GetSaramaConfig() error = %v", err)
			}
			if got.Net.TLS.Enable != tt.wantTLS {
				t.Errorf("TLS.Enable = %v, want %v", got.Net.TLS.Enable, tt.wantTLS)
			}
			if got.Net.SASL.Enable != tt.wantSASL {
				t.Errorf("SASL.Enable = %v, want %v", got.Net.SASL.Enable, tt.wantSASL)
			}
			if tt.wantSASL && got.Net.SASL.Mechanism != tt.wantMechanism {
				t.Errorf("SASL.Mechanism = %q, want %q", got.Net.SASL.Mechanism, tt.wantMechanism)
			}
		})
	}
}
