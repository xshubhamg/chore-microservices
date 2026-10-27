// Package http exposes the booking API.
package http

import (
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/xshubhamg/chore-microservices/internal/booking/app"
	"github.com/xshubhamg/chore-microservices/internal/booking/domain"
	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/config"
	"github.com/xshubhamg/chore-microservices/internal/platform/httpx"
	"github.com/xshubhamg/chore-microservices/internal/platform/idempotency"
	"log/slog"
)

const idempotencyTTL = 24 * time.Hour

type Handler struct {
	app *app.App
}

// Register mounts the booking routes. The create endpoint is protected by the
// Idempotency-Key middleware and a per-IP rate limit; reads are not.
func Register(router fiber.Router, application *app.App, cfg config.HTTPConfig, idem idempotency.Store, log *slog.Logger) {
	h := &Handler{app: application}
	group := router.Group("/bookings")

	group.Post("",
		httpx.RateLimit(cfg),
		idempotency.Middleware(idem, log, idempotencyTTL),
		h.create,
	)
	group.Get("", h.list)
	group.Get("/:id", h.get)
	group.Post("/:id/confirm", h.confirm)
	group.Post("/:id/complete", h.complete)
	group.Post("/:id/cancel", h.cancel)
}

type createRequest struct {
	ServiceID     int64     `json:"service_id"`
	CustomerName  string    `json:"customer_name"`
	CustomerEmail string    `json:"customer_email"`
	ScheduledAt   time.Time `json:"scheduled_at"`
}

type cancelRequest struct {
	Reason string `json:"reason"`
}

type bookingResponse struct {
	ID                 int64     `json:"id"`
	ServiceID          int64     `json:"service_id"`
	ServiceName        string    `json:"service_name"`
	ServicePriceCents  int64     `json:"service_price_cents"`
	DurationMinutes    int32     `json:"duration_minutes"`
	CustomerName       string    `json:"customer_name"`
	CustomerEmail      string    `json:"customer_email"`
	ScheduledAt        time.Time `json:"scheduled_at"`
	Status             string    `json:"status"`
	CancellationReason string    `json:"cancellation_reason,omitempty"`
	Version            int       `json:"version"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type singleResponse struct {
	Data bookingResponse `json:"data"`
}

type listResponse struct {
	Data       []bookingResponse `json:"data"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

func (h *Handler) create(c fiber.Ctx) error {
	var body createRequest
	if err := c.Bind().Body(&body); err != nil {
		return apperr.Invalid("invalid_json", "request body must be valid JSON").WithDetail("body", err.Error())
	}

	booking, err := h.app.Create(c.Context(), app.CreateInput{
		ServiceID:     body.ServiceID,
		CustomerName:  body.CustomerName,
		CustomerEmail: body.CustomerEmail,
		ScheduledAt:   body.ScheduledAt,
	})
	if err != nil {
		return err
	}

	c.Set(fiber.HeaderLocation, "/v1/bookings/"+strconv.FormatInt(booking.ID, 10))
	return c.Status(fiber.StatusCreated).JSON(singleResponse{Data: toResponse(booking)})
}

func (h *Handler) get(c fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}

	booking, err := h.app.Get(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(singleResponse{Data: toResponse(booking)})
}

func (h *Handler) list(c fiber.Ctx) error {
	limit := 20
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			return apperr.Invalid("invalid_limit", "limit must be between 1 and 100")
		}
		limit = parsed
	}

	after := int64(0)
	if raw := strings.TrimSpace(c.Query("after")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			return apperr.Invalid("invalid_cursor", "after must be a non-negative integer")
		}
		after = parsed
	}

	var status *domain.Status
	if raw := strings.TrimSpace(c.Query("status")); raw != "" {
		value := domain.Status(raw)
		status = &value
	}

	bookings, err := h.app.List(c.Context(), app.ListQuery{Status: status, AfterID: after, Limit: limit})
	if err != nil {
		return err
	}

	data := make([]bookingResponse, 0, len(bookings))
	for _, booking := range bookings {
		data = append(data, toResponse(booking))
	}

	nextCursor := ""
	if len(bookings) == limit && len(bookings) > 0 {
		nextCursor = strconv.FormatInt(bookings[len(bookings)-1].ID, 10)
	}

	return c.JSON(listResponse{Data: data, NextCursor: nextCursor})
}

func (h *Handler) confirm(c fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}

	booking, err := h.app.Confirm(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(singleResponse{Data: toResponse(booking)})
}

func (h *Handler) complete(c fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}

	booking, err := h.app.Complete(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(singleResponse{Data: toResponse(booking)})
}

func (h *Handler) cancel(c fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}

	var body cancelRequest
	if len(c.Request().Body()) > 0 {
		if err := c.Bind().Body(&body); err != nil {
			return apperr.Invalid("invalid_json", "request body must be valid JSON").WithDetail("body", err.Error())
		}
	}

	booking, err := h.app.Cancel(c.Context(), id, body.Reason)
	if err != nil {
		return err
	}
	return c.JSON(singleResponse{Data: toResponse(booking)})
}

func pathID(c fiber.Ctx) (int64, error) {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.Invalid("invalid_booking_id", "booking id must be a positive integer")
	}
	return id, nil
}

func toResponse(booking *domain.Booking) bookingResponse {
	return bookingResponse{
		ID:                 booking.ID,
		ServiceID:          booking.ServiceID,
		ServiceName:        booking.ServiceName,
		ServicePriceCents:  booking.ServicePriceCents,
		DurationMinutes:    booking.DurationMinutes,
		CustomerName:       booking.CustomerName,
		CustomerEmail:      booking.CustomerEmail,
		ScheduledAt:        booking.ScheduledAt,
		Status:             string(booking.Status),
		CancellationReason: booking.CancellationReason,
		Version:            booking.Version,
		CreatedAt:          booking.CreatedAt,
		UpdatedAt:          booking.UpdatedAt,
	}
}
