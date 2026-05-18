package campaigns

import (
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Status is the lifecycle state of a Campaign. Today there are two
// values; future phases (paused, archived) extend the set without
// breaking existing callers because every status that is not
// StatusActive degrades to "redirect refuses" at the boundary.
type Status string

const (
	// StatusActive marks a campaign that should serve redirects when
	// hit. The default for newly created campaigns.
	StatusActive Status = "active"

	// StatusExpired marks a campaign whose ExpiresAt has elapsed.
	// Derived state: the application transitions a campaign into
	// expired at click time when IsExpired returns true. The store
	// does not own this transition; it just persists whatever the
	// caller set.
	StatusExpired Status = "expired"
)

// Valid reports whether s is a recognised Status. Used by adapters and
// validators that scan opaque text out of storage and need to refuse
// unknown values explicitly rather than silently coerce them.
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusExpired:
		return true
	default:
		return false
	}
}

// Campaign is the aggregate root for a per-tenant UTM-tagged short
// link. The slug is the URL component the end-user clicks
// (e.g. /go/blackfriday-2026); UNIQUE (tenant_id, slug) at the schema
// level so two tenants can both own "blackfriday-2026" without
// colliding.
//
// ExpiresAt is *time.Time because evergreen campaigns never expire;
// dated promos have a hard end. The IsExpired predicate centralises
// the "should the redirect handler refuse?" question — callers must
// not duplicate the nil-check inline.
type Campaign struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Name        string
	Slug        string
	UTMSource   string
	UTMMedium   string
	UTMCampaign string
	UTMTerm     string
	UTMContent  string
	RedirectURL string
	ExpiresAt   *time.Time
	Status      Status
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewCampaign constructs a Campaign with the invariants enforced:
//   - tenantID != uuid.Nil
//   - name, slug, redirectURL all valid (slug is normalised to lower
//     case before validation so casing on input is forgiving but
//     storage is canonical)
//
// id is supplied by the caller so use-cases own uuid generation (and
// can pin it for tests). Status defaults to StatusActive; CreatedAt /
// UpdatedAt are stamped from now.
func NewCampaign(
	id uuid.UUID,
	tenantID uuid.UUID,
	name string,
	slug string,
	redirectURL string,
	expiresAt *time.Time,
	now time.Time,
) (*Campaign, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrInvalidName
	}
	normSlug, err := NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	if err := validateRedirectURL(redirectURL); err != nil {
		return nil, err
	}
	if id == uuid.Nil {
		id = uuid.New()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return &Campaign{
		ID:          id,
		TenantID:    tenantID,
		Name:        name,
		Slug:        normSlug,
		RedirectURL: redirectURL,
		ExpiresAt:   expiresAt,
		Status:      StatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// IsExpired reports whether the campaign has reached its ExpiresAt
// horizon. An evergreen campaign (ExpiresAt == nil) never expires.
//
// The comparison uses !After (i.e. now >= expiresAt) so a click that
// arrives exactly at the expiry second is rejected: marketers schedule
// "ends at midnight"; a midnight click is past the promo.
func (c *Campaign) IsExpired(now time.Time) bool {
	if c.ExpiresAt == nil {
		return false
	}
	return !now.Before(*c.ExpiresAt)
}

// WithUTM is a fluent helper that fills the five UTM fields in one
// call. Callers commonly set zero, three, or five of them; chaining
// per-field setters would be noisy. Empty strings clear the field.
func (c *Campaign) WithUTM(source, medium, campaign, term, content string) *Campaign {
	c.UTMSource = source
	c.UTMMedium = medium
	c.UTMCampaign = campaign
	c.UTMTerm = term
	c.UTMContent = content
	return c
}

// CampaignClick is one row in the click ledger. ClickID is the
// browser-supplied idempotency token; the storage adapter enforces
// UNIQUE on it so a duplicate insert (page reload, double-tap) returns
// the original row rather than creating a second one.
//
// ContactID is *uuid.UUID because most clicks arrive before the
// visitor identifies (links shared on social media land cold). It is
// linked later via Repository.LinkContactToCampaign once the visitor
// authenticates or fills out a form.
//
// IP is netip.Addr (matches pgx's native mapping to the inet column).
// The zero value (!ip.IsValid()) means "unknown" and rounds to SQL
// NULL in the adapter.
//
// Meta is an opaque per-source bag (Meta click ID, x-forwarded-for
// chain, geoip lookup result, …). Validation is deliberately at the
// application boundary; the domain refuses nil so adapters always
// have a deterministic shape to serialise.
type CampaignClick struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	CampaignID uuid.UUID
	ClickID    string
	ContactID  *uuid.UUID
	IP         netip.Addr
	UserAgent  string
	Referrer   string
	Meta       map[string]any
	CreatedAt  time.Time
}

// NewCampaignClick constructs a CampaignClick with the invariants
// enforced. clickID is REQUIRED — without it the adapter cannot
// deduplicate the second call. Use uuid.NewString() or any token the
// redirect handler can re-derive from request state.
func NewCampaignClick(
	id uuid.UUID,
	tenantID uuid.UUID,
	campaignID uuid.UUID,
	clickID string,
	now time.Time,
) (*CampaignClick, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	if campaignID == uuid.Nil {
		return nil, ErrInvalidCampaign
	}
	clickID = strings.TrimSpace(clickID)
	if clickID == "" {
		return nil, ErrInvalidClickID
	}
	if id == uuid.Nil {
		id = uuid.New()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return &CampaignClick{
		ID:         id,
		TenantID:   tenantID,
		CampaignID: campaignID,
		ClickID:    clickID,
		Meta:       map[string]any{},
		CreatedAt:  now,
	}, nil
}

// NormalizeSlug lower-cases, trims, and validates the slug. The
// allowed alphabet is a-z, 0-9, and '-'. We deliberately reject '_'
// and '.' so URLs stay consistent across the codebase ('go/X' uses
// hyphenated slugs everywhere).
//
// Empty input collapses to ErrInvalidSlug rather than passing through:
// a blank slug would route /go/ to the redirect handler with no
// campaign key, which is always a bug.
func NormalizeSlug(slug string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(slug))
	if s == "" {
		return "", ErrInvalidSlug
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return "", ErrInvalidSlug
		}
	}
	// Refuse leading/trailing hyphen — '-foo' is rarely intentional
	// and tripping it now beats hunting an ambiguity later.
	if s[0] == '-' || s[len(s)-1] == '-' {
		return "", ErrInvalidSlug
	}
	return s, nil
}

// validateRedirectURL refuses blanks and any scheme other than http /
// https. Schemes like javascript: or data: would let a malicious
// marketer plant an open-redirect-to-XSS in their own campaign URL —
// the redirect handler does an unauthenticated 302 so the URL must
// be sandboxed at write time.
func validateRedirectURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ErrInvalidRedirectURL
	}
	// Reject control characters early; url.Parse accepts some of
	// these silently and they break HTTP Location headers downstream.
	for _, r := range raw {
		if unicode.IsControl(r) {
			return ErrInvalidRedirectURL
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ErrInvalidRedirectURL
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ErrInvalidRedirectURL
	}
	if u.Host == "" {
		return ErrInvalidRedirectURL
	}
	return nil
}
