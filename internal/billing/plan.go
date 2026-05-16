package billing

import (
	"time"

	"github.com/google/uuid"
)

// Plan is an entry in the global plan catalogue. It is non-tenanted;
// master_ops curates it and app_runtime can read it to render billing pages.
// The catalogue is small and stable, so Plan is a plain value type with
// public fields — no mutation methods are needed.
type Plan struct {
	ID                uuid.UUID
	Slug              string
	Name              string
	PriceCentsBRL     int
	MonthlyTokenQuota int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
