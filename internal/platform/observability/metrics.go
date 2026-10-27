// Package observability contains the three pillars of production visibility
// used by this project: Prometheus metrics, OpenTelemetry traces and the
// middleware that connects them to HTTP, gRPC and AMQP.
package observability

import (
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	rpcBuckets  = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}
)

// Metrics owns every collector of one process and exposes a private registry,
// so the process never leaks Go runtime metrics it did not choose to expose.
type Metrics struct {
	registry *prometheus.Registry

	HTTPRequests          *prometheus.CounterVec
	HTTPDuration          *prometheus.HistogramVec
	GRPCRequests          *prometheus.CounterVec
	GRPCDuration          *prometheus.HistogramVec
	EventsPublished       *prometheus.CounterVec
	EventsConsumed        *prometheus.CounterVec
	EventDuration         *prometheus.HistogramVec
	OutboxPublishDuration prometheus.Histogram
}

func New(service, version string) *Metrics {
	reg := prometheus.NewRegistry()
	factory := promauto.With(reg)
	labels := prometheus.Labels{"service": service}

	m := &Metrics{
		registry: reg,
		HTTPRequests: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "chore",
			Name:        "http_requests_total",
			Help:        "Total number of HTTP requests.",
			ConstLabels: labels,
		}, []string{"method", "route", "status"}),
		HTTPDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace:   "chore",
			Name:        "http_request_duration_seconds",
			Help:        "HTTP request latency.",
			ConstLabels: labels,
			Buckets:     httpBuckets,
		}, []string{"method", "route"}),
		GRPCRequests: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "chore",
			Name:        "grpc_requests_total",
			Help:        "Total number of gRPC calls.",
			ConstLabels: labels,
		}, []string{"method", "code"}),
		GRPCDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace:   "chore",
			Name:        "grpc_request_duration_seconds",
			Help:        "gRPC call latency.",
			ConstLabels: labels,
			Buckets:     rpcBuckets,
		}, []string{"method"}),
		EventsPublished: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "chore",
			Name:        "events_published_total",
			Help:        "Events published to the broker by the outbox relay.",
			ConstLabels: labels,
		}, []string{"type", "result"}),
		EventsConsumed: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "chore",
			Name:        "events_consumed_total",
			Help:        "Events consumed from the broker.",
			ConstLabels: labels,
		}, []string{"type", "result"}),
		EventDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace:   "chore",
			Name:        "event_handle_duration_seconds",
			Help:        "Time spent handling one event.",
			ConstLabels: labels,
			Buckets:     rpcBuckets,
		}, []string{"type"}),
		OutboxPublishDuration: factory.NewHistogram(prometheus.HistogramOpts{
			Namespace:   "chore",
			Name:        "outbox_publish_duration_seconds",
			Help:        "Time spent publishing a single outbox message.",
			ConstLabels: labels,
			Buckets:     rpcBuckets,
		}),
	}

	factory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   "chore",
		Name:        "build_info",
		Help:        "Build information of the running binary.",
		ConstLabels: labels,
	}, []string{"version", "go_version"}).WithLabelValues(version, runtime.Version()).Set(1)

	return m
}

func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

// RegisterGaugeFunc exposes a callback-backed gauge, used for cheap periodic
// database counts such as the outbox backlog.
func (m *Metrics) RegisterGaugeFunc(name, help string, fn func() float64) error {
	return m.registry.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: "chore",
		Name:      name,
		Help:      help,
	}, fn))
}

func (m *Metrics) ObserveHTTP(method, route string, status int, duration time.Duration) {
	m.HTTPRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.HTTPDuration.WithLabelValues(method, route).Observe(duration.Seconds())
}

func (m *Metrics) ObserveGRPC(method, code string, duration time.Duration) {
	m.GRPCRequests.WithLabelValues(method, code).Inc()
	m.GRPCDuration.WithLabelValues(method).Observe(duration.Seconds())
}

func (m *Metrics) ObservePublished(eventType, result string) {
	m.EventsPublished.WithLabelValues(eventType, result).Inc()
}

func (m *Metrics) ObserveConsumed(eventType, result string, duration time.Duration) {
	m.EventsConsumed.WithLabelValues(eventType, result).Inc()
	m.EventDuration.WithLabelValues(eventType).Observe(duration.Seconds())
}
