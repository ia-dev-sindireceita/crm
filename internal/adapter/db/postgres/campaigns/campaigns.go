// Package campaigns is the pgx-backed adapter for the campaigns
// Repository port (migration 0102: campaigns + campaign_clicks).
//
// The package lives under internal/adapter/db/postgres/ so the
// forbidimport / notenant analyzers allow it to import pgx and call
// pgxpool methods directly. Every tenant-scoped call routes through
// the sibling postgres.WithTenant helper so the RLS GUC app.tenant_id
// is set before reading or writing.
//
// SIN-62954 (Fase 4 internal/campaigns, child of SIN-62197).
package campaigns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/campaigns"
)

// Compile-time assertion: Store satisfies the domain port. If the
// port grows or shrinks the build fails here before any caller
// notices.
var _ domain.Repository = (*Store)(nil)

// Store is the pgx-backed adapter for the campaigns port. Construct
// via New(pool); the pool MUST be the app_runtime pool so the RLS
// policies on campaigns / campaign_clicks apply.
type Store struct {
	pool postgres.TxBeginner
}

// New wraps pool and returns a ready-to-use Store. nil pool yields
// postgres.ErrNilPool.
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{pool: pool}, nil
}

// CreateCampaign inserts a Campaign row. The caller MUST construct
// the aggregate via domain.NewCampaign so the slug/redirect
// invariants hold; the adapter only translates UNIQUE-constraint
// errors into the typed ErrSlugAlreadyExists sentinel.
func (s *Store) CreateCampaign(ctx context.Context, c *domain.Campaign) error {
	if c == nil {
		return fmt.Errorf("campaigns/postgres: CreateCampaign: nil campaign")
	}
	if c.TenantID == uuid.Nil {
		return domain.ErrInvalidTenant
	}
	if c.ID == uuid.Nil {
		return fmt.Errorf("campaigns/postgres: CreateCampaign: id is nil")
	}
	if !c.Status.Valid() {
		return fmt.Errorf("campaigns/postgres: CreateCampaign: invalid status %q", c.Status)
	}

	err := postgres.WithTenant(ctx, s.pool, c.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO campaigns
			  (id, tenant_id, name, slug,
			   utm_source, utm_medium, utm_campaign, utm_term, utm_content,
			   redirect_url, expires_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`,
			c.ID, c.TenantID, c.Name, c.Slug,
			nullIfEmpty(c.UTMSource),
			nullIfEmpty(c.UTMMedium),
			nullIfEmpty(c.UTMCampaign),
			nullIfEmpty(c.UTMTerm),
			nullIfEmpty(c.UTMContent),
			c.RedirectURL,
			expiresAtArg(c.ExpiresAt),
			c.CreatedAt, c.UpdatedAt,
		)
		return err
	})
	if err == nil {
		return nil
	}
	if isUniqueViolation(err, "campaigns_slug_per_tenant_uniq") {
		return domain.ErrSlugAlreadyExists
	}
	return fmt.Errorf("campaigns/postgres: CreateCampaign: %w", err)
}

// GetBySlug returns the campaign row with (tenantID, slug). The slug
// is normalised before lookup so callers may pass the raw URL
// component. RLS hides rows from other tenants, so a slug owned by
// another tenant collapses to ErrNotFound just like a missing one.
func (s *Store) GetBySlug(ctx context.Context, tenantID uuid.UUID, slug string) (*domain.Campaign, error) {
	if tenantID == uuid.Nil {
		return nil, domain.ErrInvalidTenant
	}
	norm, err := domain.NormalizeSlug(slug)
	if err != nil {
		return nil, domain.ErrNotFound
	}
	var out *domain.Campaign
	err = postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, selectCampaignBySlug, norm)
		c, scanErr := scanCampaign(row)
		if scanErr != nil {
			return scanErr
		}
		out = c
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("campaigns/postgres: GetBySlug: %w", err)
	}
	return out, nil
}

// RecordClick persists the click idempotently on click_id. The
// adapter uses INSERT ... ON CONFLICT (click_id) DO NOTHING followed
// by a SELECT so concurrent callers both observe the canonical first
// row (insert wins or returns nothing; SELECT then fetches whichever
// row exists).
//
// click_id is globally UNIQUE in the schema (not per-tenant), so two
// tenants cannot both write the same token — that is intentional: a
// click_id collision across tenants would be a bug in the caller's
// generator, not a domain situation we silently merge.
func (s *Store) RecordClick(ctx context.Context, click *domain.CampaignClick) (*domain.CampaignClick, error) {
	if click == nil {
		return nil, fmt.Errorf("campaigns/postgres: RecordClick: nil click")
	}
	if click.TenantID == uuid.Nil {
		return nil, domain.ErrInvalidTenant
	}
	if click.CampaignID == uuid.Nil {
		return nil, domain.ErrInvalidCampaign
	}
	if click.ID == uuid.Nil {
		return nil, fmt.Errorf("campaigns/postgres: RecordClick: id is nil")
	}
	if click.ClickID == "" {
		return nil, domain.ErrInvalidClickID
	}

	metaJSON, err := encodeMeta(click.Meta)
	if err != nil {
		return nil, fmt.Errorf("campaigns/postgres: RecordClick: encode meta: %w", err)
	}

	var out *domain.CampaignClick
	err = postgres.WithTenant(ctx, s.pool, click.TenantID, func(tx pgx.Tx) error {
		// 1. Try the insert. ON CONFLICT (click_id) DO NOTHING means
		//    the second tenant-scoped writer simply observes no
		//    INSERT happened — we fall through to the SELECT.
		_, execErr := tx.Exec(ctx, `
			INSERT INTO campaign_clicks
			  (id, tenant_id, campaign_id, click_id, contact_id,
			   ip, user_agent, referrer, meta, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)
			ON CONFLICT (click_id) DO NOTHING
		`,
			click.ID, click.TenantID, click.CampaignID, click.ClickID,
			contactIDArg(click.ContactID),
			ipArg(click.IP),
			nullIfEmpty(click.UserAgent),
			nullIfEmpty(click.Referrer),
			metaJSON,
			click.CreatedAt,
		)
		if execErr != nil {
			return execErr
		}
		// 2. Re-read the canonical row. Either the row we just wrote
		//    or the one a concurrent caller wrote first.
		row := tx.QueryRow(ctx, selectClickByClickID, click.ClickID)
		got, scanErr := scanClick(row)
		if scanErr != nil {
			return scanErr
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("campaigns/postgres: RecordClick: %w", err)
	}
	return out, nil
}

// LinkContactToCampaign sets contact_id on the click identified by
// click_id under the tenant scope. Returns ErrNotFound when no row
// updated (either the click_id does not exist or it belongs to a
// different tenant — RLS makes both look the same to the runtime
// role, which is the desired posture).
func (s *Store) LinkContactToCampaign(ctx context.Context, tenantID uuid.UUID, clickID string, contactID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return domain.ErrInvalidTenant
	}
	if clickID == "" {
		return domain.ErrInvalidClickID
	}
	if contactID == uuid.Nil {
		return fmt.Errorf("campaigns/postgres: LinkContactToCampaign: contact id is nil")
	}
	var affected int64
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		ct, execErr := tx.Exec(ctx, `
			UPDATE campaign_clicks
			   SET contact_id = $1
			 WHERE click_id = $2
		`, contactID, clickID)
		if execErr != nil {
			return execErr
		}
		affected = ct.RowsAffected()
		return nil
	})
	if err != nil {
		return fmt.Errorf("campaigns/postgres: LinkContactToCampaign: %w", err)
	}
	if affected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ListByTenant returns every campaign for tenantID, newest-first by
// created_at. Tenants typically own a handful of campaigns per
// quarter; the call is unpaginated by design (see port comment).
func (s *Store) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*domain.Campaign, error) {
	if tenantID == uuid.Nil {
		return nil, domain.ErrInvalidTenant
	}
	var out []*domain.Campaign
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, qErr := tx.Query(ctx, selectCampaignsByTenant)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			c, scanErr := scanCampaign(rows)
			if scanErr != nil {
				return scanErr
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("campaigns/postgres: ListByTenant: %w", err)
	}
	return out, nil
}

// StatsByTenant aggregates click + attribution counters per campaign
// under tenantID. Campaigns with zero clicks are absent from the
// resulting map (the SQL GROUPs over campaign_clicks rows that exist;
// the caller must treat a missing key as the zero value).
func (s *Store) StatsByTenant(ctx context.Context, tenantID uuid.UUID) (map[uuid.UUID]domain.CampaignStats, error) {
	if tenantID == uuid.Nil {
		return nil, domain.ErrInvalidTenant
	}
	out := map[uuid.UUID]domain.CampaignStats{}
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, qErr := tx.Query(ctx, selectCampaignStatsByTenant)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			var (
				campaignID   uuid.UUID
				clicks       int64
				attributions int64
			)
			if scanErr := rows.Scan(&campaignID, &clicks, &attributions); scanErr != nil {
				return scanErr
			}
			out[campaignID] = domain.CampaignStats{Clicks: clicks, Attributions: attributions}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("campaigns/postgres: StatsByTenant: %w", err)
	}
	return out, nil
}

// defaultClickListLimit caps the click ledger drill-down when the
// caller passes a non-positive limit. Bounded so the detail view never
// streams a megabyte of click rows even when a developer forgets to
// set the page size.
const defaultClickListLimit = 100

// ListClicks returns up to limit click rows for the given campaign
// under tenantID, newest-first by created_at. A non-positive limit
// collapses to defaultClickListLimit so the SQL is always bounded.
func (s *Store) ListClicks(ctx context.Context, tenantID, campaignID uuid.UUID, limit int) ([]*domain.CampaignClick, error) {
	if tenantID == uuid.Nil {
		return nil, domain.ErrInvalidTenant
	}
	if campaignID == uuid.Nil {
		return nil, domain.ErrInvalidCampaign
	}
	if limit <= 0 {
		limit = defaultClickListLimit
	}
	var out []*domain.CampaignClick
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, qErr := tx.Query(ctx, selectClicksByCampaign, campaignID, limit)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			c, scanErr := scanClick(rows)
			if scanErr != nil {
				return scanErr
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("campaigns/postgres: ListClicks: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// SQL constants + row scanning helpers
// ---------------------------------------------------------------------------

const selectCampaignBySlug = `
	SELECT id, tenant_id, name, slug,
	       utm_source, utm_medium, utm_campaign, utm_term, utm_content,
	       redirect_url, expires_at, created_at, updated_at
	  FROM campaigns
	 WHERE slug = $1
`

const selectCampaignsByTenant = `
	SELECT id, tenant_id, name, slug,
	       utm_source, utm_medium, utm_campaign, utm_term, utm_content,
	       redirect_url, expires_at, created_at, updated_at
	  FROM campaigns
	 ORDER BY created_at DESC, id ASC
`

const selectClickByClickID = `
	SELECT id, tenant_id, campaign_id, click_id, contact_id,
	       ip, user_agent, referrer, meta, created_at
	  FROM campaign_clicks
	 WHERE click_id = $1
`

// selectCampaignStatsByTenant groups campaign_clicks by campaign_id
// and emits {clicks, attributions}. RLS scopes the rows to the
// current tenant so the GROUP BY can never accidentally aggregate
// across tenants. Campaigns with zero clicks are absent — callers
// merge against ListByTenant and default the missing key to zero.
const selectCampaignStatsByTenant = `
	SELECT campaign_id,
	       COUNT(*) AS clicks,
	       COUNT(contact_id) AS attributions
	  FROM campaign_clicks
	 GROUP BY campaign_id
`

// selectClicksByCampaign returns the most-recent click rows for one
// campaign. The campaign_id filter is the only WHERE clause beyond
// RLS — the underlying table has an index on (campaign_id, created_at
// DESC) installed by migration 0102 so the query is index-bounded.
const selectClicksByCampaign = `
	SELECT id, tenant_id, campaign_id, click_id, contact_id,
	       ip, user_agent, referrer, meta, created_at
	  FROM campaign_clicks
	 WHERE campaign_id = $1
	 ORDER BY created_at DESC, id ASC
	 LIMIT $2
`

// rowScanner is the minimal surface pgx.Row and pgx.Rows both
// satisfy. Lets scanCampaign feed both QueryRow and Query result
// iteration.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCampaign(row rowScanner) (*domain.Campaign, error) {
	var (
		c          domain.Campaign
		utmSource  *string
		utmMedium  *string
		utmCamp    *string
		utmTerm    *string
		utmContent *string
		expiresAt  *time.Time
	)
	if err := row.Scan(
		&c.ID, &c.TenantID, &c.Name, &c.Slug,
		&utmSource, &utmMedium, &utmCamp, &utmTerm, &utmContent,
		&c.RedirectURL, &expiresAt, &c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if utmSource != nil {
		c.UTMSource = *utmSource
	}
	if utmMedium != nil {
		c.UTMMedium = *utmMedium
	}
	if utmCamp != nil {
		c.UTMCampaign = *utmCamp
	}
	if utmTerm != nil {
		c.UTMTerm = *utmTerm
	}
	if utmContent != nil {
		c.UTMContent = *utmContent
	}
	c.ExpiresAt = expiresAt
	// Status is derived (not persisted in campaigns); load defaults
	// to active. The caller (use-case layer) flips to expired when
	// IsExpired returns true at click time.
	c.Status = domain.StatusActive
	return &c, nil
}

func scanClick(row rowScanner) (*domain.CampaignClick, error) {
	var (
		c         domain.CampaignClick
		contactID *uuid.UUID
		ip        *netip.Addr
		userAgent *string
		referrer  *string
		metaRaw   []byte
	)
	if err := row.Scan(
		&c.ID, &c.TenantID, &c.CampaignID, &c.ClickID,
		&contactID, &ip, &userAgent, &referrer, &metaRaw, &c.CreatedAt,
	); err != nil {
		return nil, err
	}
	c.ContactID = contactID
	if ip != nil && ip.IsValid() {
		c.IP = *ip
	}
	if userAgent != nil {
		c.UserAgent = *userAgent
	}
	if referrer != nil {
		c.Referrer = *referrer
	}
	c.Meta = decodeMeta(metaRaw)
	return &c, nil
}

// ---------------------------------------------------------------------------
// argument adapters: SQL NULL handling for optional columns
// ---------------------------------------------------------------------------

// nullIfEmpty maps an empty string to SQL NULL. The campaigns columns
// (utm_*, user_agent, referrer) are NULL-able in 0102; persisting
// empty strings would defeat readers that test IS NULL for "not
// supplied".
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// expiresAtArg returns nil for evergreen campaigns so the timestamptz
// column receives SQL NULL.
func expiresAtArg(p *time.Time) any {
	if p == nil {
		return nil
	}
	return *p
}

// contactIDArg lets pgx bind NULL when contact_id is unset.
func contactIDArg(p *uuid.UUID) any {
	if p == nil {
		return nil
	}
	return *p
}

// ipArg returns nil for the zero-valued netip.Addr (no IP recorded);
// otherwise the addr itself — pgx maps netip.Addr directly to the
// inet column without a textual round-trip.
func ipArg(addr netip.Addr) any {
	if !addr.IsValid() {
		return nil
	}
	return addr
}

// encodeMeta serialises the meta bag as JSONB bytes. A nil map
// renders the empty object {} so downstream readers can rely on
// jsonb_path_query returning rows for present keys without checking
// for null.
func encodeMeta(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte(`{}`), nil
	}
	return json.Marshal(m)
}

// decodeMeta parses the JSONB bytes back into a map. Empty / null
// payloads decode to an empty map so callers never have to nil-check.
func decodeMeta(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// isUniqueViolation reports whether err is a pgconn.PgError with
// SQLSTATE 23505 (unique_violation) on the named constraint. The
// constraint check is required because campaigns has TWO unique
// indexes (slug per tenant and the PK) and the caller routes them
// differently.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != "23505" {
		return false
	}
	return pgErr.ConstraintName == constraint
}
