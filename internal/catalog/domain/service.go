// Package domain holds the catalog invariants. It has no knowledge of HTTP,
// gRPC, SQL or any other transport.
package domain

import (
	"strings"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
)

const (
	maxNameLength        = 120
	minNameLength        = 3
	maxDescriptionLength = 1000
	maxPriceCents        = 10_000_000 // $100,000.00
	minDurationMinutes   = 15
	maxDurationMinutes   = 24 * 60
)

// Categories is the closed set of services the marketplace offers.
var Categories = map[string]struct{}{
	"cleaning":   {},
	"plumbing":   {},
	"electrical": {},
	"gardening":  {},
	"handyman":   {},
	"moving":     {},
	"painting":   {},
}

type Service struct {
	ID              int64
	Name            string
	Description     string
	Category        string
	PriceCents      int64
	DurationMinutes int32
	Active          bool

	// Version is used for optimistic concurrency control. Persistence bumps it
	// on every write.
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Details is the editable part of a service.
type Details struct {
	Name            string
	Description     string
	Category        string
	PriceCents      int64
	DurationMinutes int32
}

func New(details Details, now time.Time) (*Service, error) {
	normalized, err := normalize(details)
	if err != nil {
		return nil, err
	}

	return &Service{
		Name:            normalized.Name,
		Description:     normalized.Description,
		Category:        normalized.Category,
		PriceCents:      normalized.PriceCents,
		DurationMinutes: normalized.DurationMinutes,
		Active:          true,
		Version:         1,
		CreatedAt:       now.UTC(),
		UpdatedAt:       now.UTC(),
	}, nil
}

func (s *Service) Update(details Details, now time.Time) error {
	normalized, err := normalize(details)
	if err != nil {
		return err
	}

	s.Name = normalized.Name
	s.Description = normalized.Description
	s.Category = normalized.Category
	s.PriceCents = normalized.PriceCents
	s.DurationMinutes = normalized.DurationMinutes
	s.UpdatedAt = now.UTC()
	return nil
}

// SetActive activates or deactivates a service. Deactivation is a soft delete:
// historical bookings keep their snapshot of the service.
func (s *Service) SetActive(active bool, now time.Time) {
	if s.Active == active {
		return
	}
	s.Active = active
	s.UpdatedAt = now.UTC()
}

func normalize(details Details) (Details, error) {
	details.Name = strings.TrimSpace(details.Name)
	details.Description = strings.TrimSpace(details.Description)
	details.Category = strings.ToLower(strings.TrimSpace(details.Category))

	err := apperr.Invalid("invalid_service", "service details are invalid")

	switch {
	case len(details.Name) < minNameLength:
		err.WithDetail("name", "must be at least 3 characters")
	case len(details.Name) > maxNameLength:
		err.WithDetail("name", "must be at most 120 characters")
	}

	if len(details.Description) > maxDescriptionLength {
		err.WithDetail("description", "must be at most 1000 characters")
	}

	if _, ok := Categories[details.Category]; !ok {
		err.WithDetail("category", "must be one of cleaning, plumbing, electrical, gardening, handyman, moving, painting")
	}

	if details.PriceCents < 0 {
		err.WithDetail("price_cents", "must be zero or greater")
	}
	if details.PriceCents > maxPriceCents {
		err.WithDetail("price_cents", "must be at most 10000000 cents")
	}

	if details.DurationMinutes < minDurationMinutes || details.DurationMinutes > maxDurationMinutes {
		err.WithDetail("duration_minutes", "must be between 15 and 1440 minutes")
	}

	if len(err.Details) > 0 {
		return Details{}, err
	}
	return details, nil
}
