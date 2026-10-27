// Package grpc exposes the catalog over gRPC for internal service-to-service
// calls.
package grpc

import (
	"context"

	"github.com/xshubhamg/chore-microservices/internal/catalog/app"
	catalogv1 "github.com/xshubhamg/chore-microservices/internal/gen/catalog/v1"
	"github.com/xshubhamg/chore-microservices/internal/platform/grpcx"
)

type Server struct {
	catalogv1.UnimplementedCatalogServiceServer
	app *app.App
}

func NewServer(application *app.App) *Server {
	return &Server{app: application}
}

func (s *Server) GetService(ctx context.Context, req *catalogv1.GetServiceRequest) (*catalogv1.GetServiceResponse, error) {
	service, err := s.app.Get(ctx, req.GetId())
	if err != nil {
		return nil, grpcx.ToStatus(err)
	}

	return &catalogv1.GetServiceResponse{
		Service: &catalogv1.Service{
			Id:              service.ID,
			Name:            service.Name,
			Description:     service.Description,
			Category:        service.Category,
			PriceCents:      service.PriceCents,
			DurationMinutes: service.DurationMinutes,
			Active:          service.Active,
		},
	}, nil
}
