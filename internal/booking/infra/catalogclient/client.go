// Package catalogclient is the booking service's adapter for the catalog gRPC
// API. It translates transport errors into the shared application error model.
package catalogclient

import (
	"context"
	"errors"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/booking/app"
	catalogv1 "github.com/xshubhamg/chore-microservices/internal/gen/catalog/v1"
	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/grpcx"
	"google.golang.org/grpc"
)

type Client struct {
	client  catalogv1.CatalogServiceClient
	timeout time.Duration
}

func New(conn *grpc.ClientConn, timeout time.Duration) *Client {
	return &Client{
		client:  catalogv1.NewCatalogServiceClient(conn),
		timeout: timeout,
	}
}

func (c *Client) GetService(ctx context.Context, id int64) (app.CatalogService, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.client.GetService(ctx, &catalogv1.GetServiceRequest{Id: id})
	if err != nil {
		return app.CatalogService{}, grpcx.FromStatus(err)
	}

	service := resp.GetService()
	if service == nil {
		return app.CatalogService{}, apperr.Internal(errors.New("catalog returned an empty service"))
	}

	return app.CatalogService{
		ID:              service.GetId(),
		Name:            service.GetName(),
		PriceCents:      service.GetPriceCents(),
		DurationMinutes: service.GetDurationMinutes(),
		Active:          service.GetActive(),
	}, nil
}
