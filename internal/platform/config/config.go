// Package config loads and validates the 12-factor configuration shared by all
// services. Every value comes from the environment, so the same binary runs
// locally, in CI and in production without changes.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	EnvDevelopment = "dev"
	EnvTest        = "test"
	EnvProduction  = "production"
)

type Config struct {
	Service         string
	Env             string
	Version         string
	AutoMigrate     bool
	ShutdownTimeout time.Duration

	Log   LogConfig
	HTTP  HTTPConfig
	Admin AdminConfig
	GRPC  GRPCConfig
	DB    DBConfig
	AMQP  AMQPConfig

	Catalog   CatalogConfig
	Outbox    OutboxConfig
	Telemetry TelemetryConfig
}

type LogConfig struct {
	Level slog.Level
}

type HTTPConfig struct {
	Addr               string
	ReadTimeout        time.Duration
	WriteTimeout       time.Duration
	IdleTimeout        time.Duration
	RequestTimeout     time.Duration
	BodyLimit          int
	RateLimitPerMinute int
}

type AdminConfig struct {
	Addr string
}

type GRPCConfig struct {
	Addr string
}

type DBConfig struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
}

type AMQPConfig struct {
	URL      string
	Exchange string
	Prefetch int
}

type CatalogConfig struct {
	GRPCAddr string
	Timeout  time.Duration
}

type OutboxConfig struct {
	PollInterval time.Duration
	BatchSize    int
	MaxAttempts  int
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
}

type TelemetryConfig struct {
	OTLPEndpoint string
	SampleRatio  float64
	Insecure     bool
}

// Load reads configuration for the given service from the environment.
func Load(service string) (Config, error) {
	l := &loader{}
	env := l.str("ENV", EnvDevelopment)

	cfg := Config{
		Service:         service,
		Env:             env,
		Version:         l.str("VERSION", "dev"),
		AutoMigrate:     l.boolean("DB_AUTO_MIGRATE", true),
		ShutdownTimeout: l.duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		Log: LogConfig{
			Level: l.level("LOG_LEVEL", slog.LevelInfo),
		},
		HTTP: HTTPConfig{
			Addr:               l.str("HTTP_ADDR", ":8080"),
			ReadTimeout:        l.duration("HTTP_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:       l.duration("HTTP_WRITE_TIMEOUT", 15*time.Second),
			IdleTimeout:        l.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			RequestTimeout:     l.duration("HTTP_REQUEST_TIMEOUT", 10*time.Second),
			BodyLimit:          l.integer("HTTP_BODY_LIMIT_BYTES", 1<<20),
			RateLimitPerMinute: l.integer("HTTP_RATE_LIMIT_PER_MINUTE", 0),
		},
		Admin: AdminConfig{
			Addr: l.str("ADMIN_ADDR", ":9090"),
		},
		GRPC: GRPCConfig{
			Addr: l.str("GRPC_ADDR", ":50051"),
		},
		DB: DBConfig{
			DSN:             l.str("DATABASE_URL", fmt.Sprintf("postgres://chore:chore@localhost:5435/chore_%s?sslmode=disable", service)),
			MaxConns:        l.integer32("DB_MAX_CONNS", 10),
			MinConns:        l.integer32("DB_MIN_CONNS", 1),
			MaxConnLifetime: l.duration("DB_MAX_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime: l.duration("DB_MAX_CONN_IDLE_TIME", 15*time.Minute),
			ConnectTimeout:  l.duration("DB_CONNECT_TIMEOUT", 30*time.Second),
		},
		AMQP: AMQPConfig{
			URL:      l.str("RABBITMQ_URL", "amqp://chore:chore@localhost:5672/"),
			Exchange: l.str("RABBITMQ_EXCHANGE", "chore.events"),
			Prefetch: l.integer("RABBITMQ_PREFETCH", 20),
		},
		Catalog: CatalogConfig{
			GRPCAddr: l.str("CATALOG_GRPC_ADDR", "localhost:50051"),
			Timeout:  l.duration("CATALOG_GRPC_TIMEOUT", 2*time.Second),
		},
		Outbox: OutboxConfig{
			PollInterval: l.duration("OUTBOX_POLL_INTERVAL", 500*time.Millisecond),
			BatchSize:    l.integer("OUTBOX_BATCH_SIZE", 50),
			MaxAttempts:  l.integer("OUTBOX_MAX_ATTEMPTS", 10),
			BaseBackoff:  l.duration("OUTBOX_BASE_BACKOFF", 2*time.Second),
			MaxBackoff:   l.duration("OUTBOX_MAX_BACKOFF", 5*time.Minute),
		},
		Telemetry: TelemetryConfig{
			OTLPEndpoint: l.str("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
			SampleRatio:  l.float("OTEL_SAMPLE_RATIO", 1),
			Insecure:     l.boolean("OTEL_EXPORTER_OTLP_INSECURE", true),
		},
	}

	if err := l.err(); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) IsProduction() bool { return c.Env == EnvProduction }

func (c Config) validate() error {
	var problems []error
	if strings.TrimSpace(c.Service) == "" {
		problems = append(problems, errors.New("service name is required"))
	}
	if strings.TrimSpace(c.HTTP.Addr) == "" {
		problems = append(problems, errors.New("HTTP_ADDR is required"))
	}
	if c.HTTP.RequestTimeout <= 0 || c.HTTP.ReadTimeout <= 0 || c.HTTP.WriteTimeout <= 0 {
		problems = append(problems, errors.New("http timeouts must be positive"))
	}
	if c.HTTP.BodyLimit <= 0 {
		problems = append(problems, errors.New("HTTP_BODY_LIMIT_BYTES must be positive"))
	}
	if c.DB.MaxConns < 1 || c.DB.MinConns < 0 || c.DB.MinConns > c.DB.MaxConns {
		problems = append(problems, errors.New("invalid database pool sizing"))
	}
	if _, err := url.Parse(c.DB.DSN); err != nil {
		problems = append(problems, fmt.Errorf("invalid DATABASE_URL: %w", err))
	}
	if c.Outbox.BatchSize < 1 {
		problems = append(problems, errors.New("OUTBOX_BATCH_SIZE must be positive"))
	}
	if c.Outbox.MaxAttempts < 1 {
		problems = append(problems, errors.New("OUTBOX_MAX_ATTEMPTS must be positive"))
	}
	if c.Telemetry.SampleRatio < 0 || c.Telemetry.SampleRatio > 1 {
		problems = append(problems, errors.New("OTEL_SAMPLE_RATIO must be between 0 and 1"))
	}
	return errors.Join(problems...)
}

var passwordPattern = regexp.MustCompile(`(password=)[^\s&]+`)

// RedactedDSN is safe to log: it hides the password in both URL and keyword
// DSN formats.
func RedactedDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
		return u.String()
	}
	return passwordPattern.ReplaceAllString(dsn, "${1}xxxxx")
}

type loader struct {
	problems []error
}

func (l *loader) err() error { return errors.Join(l.problems...) }

func (l *loader) str(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func (l *loader) integer(key string, fallback int) int {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		l.problems = append(l.problems, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return v
}

// integer32 keeps the conversion inside a bounds-checked parse. This avoids a
// silent wraparound when an operator sets a value outside the int32 range.
func (l *loader) integer32(key string, fallback int32) int32 {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	v, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		l.problems = append(l.problems, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return int32(v)
}

func (l *loader) float(key string, fallback float64) float64 {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		l.problems = append(l.problems, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return v
}

func (l *loader) boolean(key string, fallback bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		l.problems = append(l.problems, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return v
}

func (l *loader) duration(key string, fallback time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		l.problems = append(l.problems, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return v
}

func (l *loader) level(key string, fallback slog.Level) slog.Level {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		l.problems = append(l.problems, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return level
}
