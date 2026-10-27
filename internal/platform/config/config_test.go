package config_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/platform/config"
)

var allKeys = []string{
	"ENV", "VERSION", "LOG_LEVEL", "HTTP_ADDR", "ADMIN_ADDR", "GRPC_ADDR",
	"HTTP_REQUEST_TIMEOUT", "HTTP_BODY_LIMIT_BYTES", "HTTP_RATE_LIMIT_PER_MINUTE",
	"SHUTDOWN_TIMEOUT", "DB_AUTO_MIGRATE", "DATABASE_URL", "DB_MAX_CONNS",
	"DB_MIN_CONNS", "DB_CONNECT_TIMEOUT", "RABBITMQ_URL", "RABBITMQ_EXCHANGE",
	"CATALOG_GRPC_ADDR", "OUTBOX_BATCH_SIZE", "OUTBOX_MAX_ATTEMPTS",
	"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_SAMPLE_RATIO", "OTEL_EXPORTER_OTLP_INSECURE",
}

func isolateEnv(t *testing.T) {
	t.Helper()
	for _, key := range allKeys {
		previous, existed := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(key, previous)
				return
			}
			_ = os.Unsetenv(key)
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	isolateEnv(t)

	cfg, err := config.Load("booking")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Service != "booking" {
		t.Errorf("service = %q", cfg.Service)
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("http addr = %q, want :8080", cfg.HTTP.Addr)
	}
	if !strings.Contains(cfg.DB.DSN, "chore_booking") || !strings.Contains(cfg.DB.DSN, ":5435") {
		t.Errorf("dsn = %q, want local booking database on port 5435", cfg.DB.DSN)
	}
	if !cfg.AutoMigrate {
		t.Error("auto migrate should default to true")
	}
	if cfg.Telemetry.SampleRatio != 1 {
		t.Errorf("sample ratio = %v, want 1", cfg.Telemetry.SampleRatio)
	}
	if cfg.Outbox.MaxAttempts != 10 {
		t.Errorf("max attempts = %d, want 10", cfg.Outbox.MaxAttempts)
	}
}

func TestLoadOverrides(t *testing.T) {
	isolateEnv(t)

	t.Setenv("HTTP_ADDR", ":9999")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("SHUTDOWN_TIMEOUT", "5s")
	t.Setenv("DB_MAX_CONNS", "42")
	t.Setenv("OTEL_SAMPLE_RATIO", "0.25")
	t.Setenv("DB_AUTO_MIGRATE", "false")

	cfg, err := config.Load("catalog")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.HTTP.Addr != ":9999" {
		t.Errorf("http addr = %q", cfg.HTTP.Addr)
	}
	if cfg.Log.Level.String() != "DEBUG" {
		t.Errorf("log level = %q", cfg.Log.Level)
	}
	if cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("shutdown timeout = %v", cfg.ShutdownTimeout)
	}
	if cfg.DB.MaxConns != 42 {
		t.Errorf("max conns = %d", cfg.DB.MaxConns)
	}
	if cfg.Telemetry.SampleRatio != 0.25 {
		t.Errorf("sample ratio = %v", cfg.Telemetry.SampleRatio)
	}
	if cfg.AutoMigrate {
		t.Error("auto migrate should be disabled")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"bad duration", "HTTP_REQUEST_TIMEOUT", "soon"},
		{"bad integer", "DB_MAX_CONNS", "many"},
		{"bad bool", "DB_AUTO_MIGRATE", "maybe"},
		{"sample out of range", "OTEL_SAMPLE_RATIO", "2"},
		{"negative pool", "DB_MAX_CONNS", "-1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv(tc.key, tc.value)

			if _, err := config.Load("booking"); err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
}

func TestRedactedDSN(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "url",
			dsn:  "postgres://chore:secret@localhost:5435/chore_booking?sslmode=disable",
			want: "postgres://chore:xxxxx@localhost:5435/chore_booking?sslmode=disable",
		},
		{
			name: "keyword",
			dsn:  "host=localhost user=chore password=secret dbname=chore",
			want: "host=localhost user=chore password=xxxxx dbname=chore",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := config.RedactedDSN(tc.dsn); got != tc.want {
				t.Errorf("redacted = %q, want %q", got, tc.want)
			}
		})
	}
}
