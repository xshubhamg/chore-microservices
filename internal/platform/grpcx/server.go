// Package grpcx wires gRPC with the same observability and error model as the
// HTTP transport.
package grpcx

import (
	"context"
	"log/slog"
	"net"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
)

type Server struct {
	server *grpc.Server
	addr   string
	log    *slog.Logger
}

// NewServer builds a gRPC server with panic recovery, request logging, RED
// metrics and OpenTelemetry tracing.
func NewServer(addr string, log *slog.Logger, metrics *observability.Metrics) *Server {
	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(
			recoveryInterceptor(log),
			loggingInterceptor(log),
			metricsInterceptor(metrics),
		),
	)

	return &Server{server: srv, addr: addr, log: log}
}

func (s *Server) GRPC() *grpc.Server { return s.server }

func (s *Server) Start(context.Context) error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}

	s.log.Info("grpc server listening", slog.String("addr", listener.Addr().String()))

	if err := s.server.Serve(listener); err != nil && err != grpc.ErrServerStopped {
		return err
	}
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.server.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.server.Stop()
		return ctx.Err()
	}
}

func recoveryInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error("panic in grpc handler",
					slog.String("method", info.FullMethod),
					slog.Any("panic", recovered),
				)
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

func loggingInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)

		attrs := []any{
			slog.String("method", info.FullMethod),
			slog.Duration("duration", time.Since(start)),
			slog.String("code", status.Code(err).String()),
		}
		if err != nil {
			log.With(attrs...).Warn("grpc request failed", slog.String("error", err.Error()))
		} else {
			log.With(attrs...).Info("grpc request completed")
		}
		return resp, err
	}
}

func metricsInterceptor(metrics *observability.Metrics) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		metrics.ObserveGRPC(info.FullMethod, status.Code(err).String(), time.Since(start))
		return resp, err
	}
}

// clientMetricsInterceptor is unexported because clients are constructed in
// Dial.
func clientMetricsInterceptor(metrics *observability.Metrics) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, conn *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		start := time.Now()
		err := invoker(ctx, method, req, reply, conn, opts...)
		metrics.ObserveGRPC(method, status.Code(err).String(), time.Since(start))
		return err
	}
}
