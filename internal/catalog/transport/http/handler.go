// Package http exposes the catalog over REST.
package http

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/xshubhamg/chore-microservices/internal/catalog/app"
	"github.com/xshubhamg/chore-microservices/internal/catalog/domain"
	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/config"
	"github.com/xshubhamg/chore-microservices/internal/platform/httpx"
)

type Handler struct {
	app *app.App
}

func Register(router fiber.Router, application *app.App, cfg config.HTTPConfig) {
	h := &Handler{app: application}
	group := router.Group("/services")

	group.Post("", httpx.RateLimit(cfg), h.create)
	group.Get("", h.list)
	group.Get("/:id", h.get)
	group.Put("/:id", h.update)
	group.Put("/:id/active", h.setActive)
}

type writeRequest struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	Category        string `json:"category"`
	PriceCents      int64  `json:"price_cents"`
	DurationMinutes int32  `json:"duration_minutes"`
}

type activeRequest struct {
	Active *bool `json:"active"`
}

type serviceResponse struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	Category        string `json:"category"`
	PriceCents      int64  `json:"price_cents"`
	DurationMinutes int32  `json:"duration_minutes"`
	Active          bool   `json:"active"`
	Version         int    `json:"version"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

type singleResponse struct {
	Data serviceResponse `json:"data"`
}

type listResponse struct {
	Data       []serviceResponse `json:"data"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

func (h *Handler) create(c fiber.Ctx) error {
	var body writeRequest
	if err := c.Bind().Body(&body); err != nil {
		return apperr.Invalid("invalid_json", "request body must be valid JSON").WithDetail("body", err.Error())
	}

	created, err := h.app.Create(c.Context(), app.CreateInput{
		Name:            body.Name,
		Description:     body.Description,
		Category:        body.Category,
		PriceCents:      body.PriceCents,
		DurationMinutes: body.DurationMinutes,
	})
	if err != nil {
		return err
	}

	c.Set(fiber.HeaderLocation, "/v1/services/"+strconv.FormatInt(created.ID, 10))
	return c.Status(fiber.StatusCreated).JSON(singleResponse{Data: toResponse(created)})
}

func (h *Handler) get(c fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}

	service, err := h.app.Get(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(singleResponse{Data: toResponse(service)})
}

func (h *Handler) list(c fiber.Ctx) error {
	limit, err := queryLimit(c)
	if err != nil {
		return err
	}
	after, err := queryCursor(c)
	if err != nil {
		return err
	}

	var active *bool
	if raw := c.Query("active"); raw != "" {
		value, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return apperr.Invalid("invalid_filter", "active must be true or false").WithDetail("active", raw)
		}
		active = &value
	}

	services, err := h.app.List(c.Context(), app.ListQuery{Active: active, AfterID: after, Limit: limit})
	if err != nil {
		return err
	}

	data := make([]serviceResponse, 0, len(services))
	for _, service := range services {
		data = append(data, toResponse(service))
	}

	nextCursor := ""
	if len(services) == limit && len(services) > 0 {
		nextCursor = strconv.FormatInt(services[len(services)-1].ID, 10)
	}

	return c.JSON(listResponse{Data: data, NextCursor: nextCursor})
}

func (h *Handler) update(c fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}

	var body writeRequest
	if err := c.Bind().Body(&body); err != nil {
		return apperr.Invalid("invalid_json", "request body must be valid JSON").WithDetail("body", err.Error())
	}

	updated, err := h.app.Update(c.Context(), id, app.UpdateInput{
		Name:            body.Name,
		Description:     body.Description,
		Category:        body.Category,
		PriceCents:      body.PriceCents,
		DurationMinutes: body.DurationMinutes,
	})
	if err != nil {
		return err
	}
	return c.JSON(singleResponse{Data: toResponse(updated)})
}

func (h *Handler) setActive(c fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}

	var body activeRequest
	if err := c.Bind().Body(&body); err != nil {
		return apperr.Invalid("invalid_json", "request body must be valid JSON").WithDetail("body", err.Error())
	}
	if body.Active == nil {
		return apperr.Invalid("missing_active", "active is required").WithDetail("active", "required")
	}

	updated, err := h.app.SetActive(c.Context(), id, *body.Active)
	if err != nil {
		return err
	}
	return c.JSON(singleResponse{Data: toResponse(updated)})
}

func pathID(c fiber.Ctx) (int64, error) {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.Invalid("invalid_service_id", "service id must be a positive integer")
	}
	return id, nil
}

func queryLimit(c fiber.Ctx) (int, error) {
	raw := strings.TrimSpace(c.Query("limit"))
	if raw == "" {
		return 20, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 100 {
		return 0, apperr.Invalid("invalid_limit", "limit must be between 1 and 100")
	}
	return limit, nil
}

func queryCursor(c fiber.Ctx) (int64, error) {
	raw := strings.TrimSpace(c.Query("after"))
	if raw == "" {
		return 0, nil
	}
	after, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || after < 0 {
		return 0, apperr.Invalid("invalid_cursor", "after must be a non-negative integer")
	}
	return after, nil
}

func toResponse(service *domain.Service) serviceResponse {
	return serviceResponse{
		ID:              service.ID,
		Name:            service.Name,
		Description:     service.Description,
		Category:        service.Category,
		PriceCents:      service.PriceCents,
		DurationMinutes: service.DurationMinutes,
		Active:          service.Active,
		Version:         service.Version,
		CreatedAt:       service.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:       service.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}
