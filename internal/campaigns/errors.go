package campaigns

import "errors"

// ErrInvalidTenant is returned when tenantID is uuid.Nil. Every
// campaign row is tenant-scoped; an anonymous campaign is a
// programming bug, not a runtime input we tolerate.
var ErrInvalidTenant = errors.New("campaigns: invalid tenant id")

// ErrInvalidCampaign is returned when a campaign id is uuid.Nil where
// the caller must address an existing aggregate.
var ErrInvalidCampaign = errors.New("campaigns: invalid campaign id")

// ErrInvalidName is returned when the marketer-provided campaign
// name is blank after trimming. Storage NOT NULL would reject it
// downstream — surface it cleanly at the domain edge instead.
var ErrInvalidName = errors.New("campaigns: name is required")

// ErrInvalidSlug is returned when the slug is blank or contains
// characters outside the URL-safe lowercase set (a-z, 0-9, -).
// Marketers paste slugs straight into /go/<slug>; we refuse anything
// the redirect handler cannot route safely.
var ErrInvalidSlug = errors.New("campaigns: invalid slug")

// ErrInvalidRedirectURL is returned when the redirect target is blank
// or lacks an http/https scheme. The redirect handler does a 302 to
// this URL; arbitrary javascript: / data: schemes are an open-redirect
// vector and must be filtered at the boundary.
var ErrInvalidRedirectURL = errors.New("campaigns: invalid redirect url")

// ErrInvalidClickID is returned when a CampaignClick is constructed
// without a click_id. The schema makes click_id UNIQUE NOT NULL — the
// browser/redirect handler MUST supply a token (uuid, hex, etc.) so a
// page reload deduplicates.
var ErrInvalidClickID = errors.New("campaigns: invalid click id")

// ErrSlugAlreadyExists is returned when CreateCampaign would violate
// the UNIQUE (tenant_id, slug) constraint from 0102. Callers can
// errors.Is against it to render a 409 / "slug taken" without parsing
// pgx error codes.
var ErrSlugAlreadyExists = errors.New("campaigns: slug already exists for tenant")

// ErrNotFound is the storage-layer sentinel for "no row matched".
// RLS-hidden rows from other tenants collapse to the same sentinel so
// an adversary cannot tell "exists under another tenant" from "does
// not exist".
var ErrNotFound = errors.New("campaigns: not found")
