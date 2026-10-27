package grpcx

import (
	"log/slog"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
)

// retryServiceConfig uses gRPC's built-in retry policy for idempotent reads.
// GetService is safe to retry, so transient UNAVAILABLE errors are retried with
// exponential backoff before the caller sees them.
const retryServiceConfig = `{
  "methodConfig": [{
    "name": [{"service": "catalog.v1.CatalogService", "method": "GetService"}],
    "retryPolicy": {
      "maxAttempts": 3,
      "initialBackoff": "0.1s",
      "maxBackoff": "1s",
      "backoffMultiplier": 2,
      "retryableStatusCodes": ["UNAVAILABLE", "DEADLINE_EXCEEDED"]
    }
  }]
}`

// Dial creates a lazy client connection to an internal service.
//
// Internal traffic uses plaintext because the services run inside a private
// network namespace; production deployments should enable mTLS (or a service
// mesh) here.
func Dial(addr string, log *slog.Logger, metrics *observability.Metrics) (*grpc.ClientConn, error) {
	return grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithDefaultServiceConfig(retryServiceConfig),
		grpc.WithChainUnaryInterceptor(
			clientMetricsInterceptor(metrics),
		),
	)
}
