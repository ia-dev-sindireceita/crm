package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/customdomain/management"
)

// CustomDomainStore is the write-side adapter for tenant_custom_domains
// (migration 0010). It is the production implementation of
// management.Store.
//
// Reads of multiple rows go through Query; the existing PgxConn surface
// only declares QueryRow + Exec. PgxRowsConn is the narrowed pgx.Pool
// shape this store needs.
type CustomDomainStore struct {
	db PgxRowsConn
}

// PgxRowsConn extends PgxConn with Query for multi-row reads. *pgxpool.Pool
// satisfies it; tests use a stub.
type PgxRowsConn interface {
	PgxConn
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// NewCustomDomainStore returns a store bound to db.
func NewCustomDomainStore(db PgxRowsConn) *CustomDomainStore { return &CustomDomainStore{db: db} }

// listSQL returns active rows newest-first per the
// idx_tenant_custom_domains_created_at partial index. Soft-deleted rows
// are excluded — UI lists never display them.
const customDomainListSQL = `
SELECT id, tenant_id, host, verification_token, verified_at, verified_with_dnssec,
       tls_paused_at, deleted_at, dns_resolution_log_id, created_at, updated_at
  FROM tenant_custom_domains
 WHERE tenant_id = $1 AND deleted_at IS NULL
 ORDER BY created_at DESC
`

// List returns active domains for tenantID. Empty result is a non-error
// `nil, nil` so the template can render "no domains yet" deterministically.
func (s *CustomDomainStore) List(ctx context.Context, tenantID uuid.UUID) ([]management.Domain, error) {
	rows, err := s.db.Query(ctx, customDomainListSQL, tenantID)
	if err != nil {
		return nil, fmt.Errorf("custom_domain list: %w", err)
	}
	defer rows.Close()
	var out []management.Domain
	for rows.Next() {
		d, err := scanCustomDomain(rows)
		if err != nil {
			return nil, fmt.Errorf("custom_domain list scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("custom_domain list rows: %w", err)
	}
	return out, nil
}

const customDomainGetSQL = `
SELECT id, tenant_id, host, verification_token, verified_at, verified_with_dnssec,
       tls_paused_at, deleted_at, dns_resolution_log_id, created_at, updated_at
  FROM tenant_custom_domains
 WHERE id = $1
 LIMIT 1
`

// GetByID returns a single row by id, including soft-deleted rows so the
// management layer can detect "domain was deleted" rather than 404.
func (s *CustomDomainStore) GetByID(ctx context.Context, id uuid.UUID) (management.Domain, error) {
	row := s.db.QueryRow(ctx, customDomainGetSQL, id)
	return scanCustomDomainRow(row)
}

const customDomainInsertSQL = `
INSERT INTO tenant_custom_domains (id, tenant_id, host, verification_token, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $5)
RETURNING id, tenant_id, host, verification_token, verified_at, verified_with_dnssec,
          tls_paused_at, deleted_at, dns_resolution_log_id, created_at, updated_at
`

// Insert appends a new row. The unique index on lower(host) WHERE
// deleted_at IS NULL surfaces conflicts as 23505.
func (s *CustomDomainStore) Insert(ctx context.Context, d management.Domain) (management.Domain, error) {
	row := s.db.QueryRow(ctx, customDomainInsertSQL, d.ID, d.TenantID, d.Host, d.VerificationToken, d.CreatedAt)
	return scanCustomDomainRow(row)
}

const customDomainMarkVerifiedSQL = `
UPDATE tenant_custom_domains
   SET verified_at = $2,
       verified_with_dnssec = $3,
       dns_resolution_log_id = $4,
       updated_at = $2
 WHERE id = $1 AND deleted_at IS NULL
RETURNING id, tenant_id, host, verification_token, verified_at, verified_with_dnssec,
          tls_paused_at, deleted_at, dns_resolution_log_id, created_at, updated_at
`

// MarkVerified flips verified_at + verified_with_dnssec.
func (s *CustomDomainStore) MarkVerified(ctx context.Context, id uuid.UUID, at time.Time, withDNSSEC bool, dnsLogID *uuid.UUID) (management.Domain, error) {
	var dnsArg any
	if dnsLogID != nil {
		dnsArg = *dnsLogID
	}
	row := s.db.QueryRow(ctx, customDomainMarkVerifiedSQL, id, at, withDNSSEC, dnsArg)
	return scanCustomDomainRow(row)
}

const customDomainSetPausedSQL = `
UPDATE tenant_custom_domains
   SET tls_paused_at = $2,
       updated_at = $3
 WHERE id = $1 AND deleted_at IS NULL
RETURNING id, tenant_id, host, verification_token, verified_at, verified_with_dnssec,
          tls_paused_at, deleted_at, dns_resolution_log_id, created_at, updated_at
`

// SetPaused sets tls_paused_at to *pausedAt or NULL.
func (s *CustomDomainStore) SetPaused(ctx context.Context, id uuid.UUID, pausedAt *time.Time) (management.Domain, error) {
	var pausedArg any
	now := time.Now().UTC()
	if pausedAt != nil {
		pausedArg = *pausedAt
		now = *pausedAt
	}
	row := s.db.QueryRow(ctx, customDomainSetPausedSQL, id, pausedArg, now)
	return scanCustomDomainRow(row)
}

const customDomainSoftDeleteSQL = `
UPDATE tenant_custom_domains
   SET deleted_at = $2,
       updated_at = $2
 WHERE id = $1 AND deleted_at IS NULL
RETURNING id, tenant_id, host, verification_token, verified_at, verified_with_dnssec,
          tls_paused_at, deleted_at, dns_resolution_log_id, created_at, updated_at
`

// SoftDelete flips deleted_at. The partial unique index on (lower(host),
// deleted_at IS NULL) automatically frees the host for re-claim.
func (s *CustomDomainStore) SoftDelete(ctx context.Context, id uuid.UUID, at time.Time) (management.Domain, error) {
	row := s.db.QueryRow(ctx, customDomainSoftDeleteSQL, id, at)
	return scanCustomDomainRow(row)
}

// scanCustomDomain wraps a *pgx.Rows into the same row shape scanCustomDomainRow expects.
func scanCustomDomain(rows pgx.Rows) (management.Domain, error) {
	return scanCustomDomainRow(rows)
}

// customDomainListPendingSQL returns rows the SIN-63080 verifier worker
// should poll: not deleted, not yet verified, not paused, not already
// failed. The partial index idx_tenant_custom_domains_pending_verification
// keeps this scan cheap even when the table grows. Ordered oldest-first
// so a freshly enrolled domain gets attention promptly while a long-
// pending backlog is processed FIFO.
//
// The projection includes failed_at + failure_reason so the worker can
// stay defensive against rows another process raced into the failed
// state between the index lookup and the scan.
const customDomainListPendingSQL = `
SELECT id, tenant_id, host, verification_token, verified_at, verified_with_dnssec,
       tls_paused_at, deleted_at, failed_at, failure_reason, dns_resolution_log_id, created_at, updated_at
  FROM tenant_custom_domains
 WHERE deleted_at      IS NULL
   AND verified_at     IS NULL
   AND failed_at       IS NULL
   AND tls_paused_at   IS NULL
 ORDER BY created_at ASC
`

// ListPendingVerification returns rows the customdomain-verifier worker
// should poll for TXT propagation. The result set crosses tenants — the
// worker runs as a single deployment-wide process and bypasses RLS via
// the master pool (same posture as the dunning worker).
func (s *CustomDomainStore) ListPendingVerification(ctx context.Context) ([]management.Domain, error) {
	rows, err := s.db.Query(ctx, customDomainListPendingSQL)
	if err != nil {
		return nil, fmt.Errorf("custom_domain list pending: %w", err)
	}
	defer rows.Close()
	var out []management.Domain
	for rows.Next() {
		d, err := scanCustomDomainRowWithFailure(rows)
		if err != nil {
			return nil, fmt.Errorf("custom_domain list pending scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("custom_domain list pending rows: %w", err)
	}
	return out, nil
}

const customDomainMarkFailedSQL = `
UPDATE tenant_custom_domains
   SET failed_at      = $2,
       failure_reason = $3,
       updated_at     = $2
 WHERE id = $1 AND deleted_at IS NULL
RETURNING id, tenant_id, host, verification_token, verified_at, verified_with_dnssec,
          tls_paused_at, deleted_at, failed_at, failure_reason, dns_resolution_log_id, created_at, updated_at
`

// MarkFailed flips failed_at + failure_reason. Called by the verifier
// worker after exhausting its in-memory attempt cap. Idempotent at the
// schema level — a second call against an already-failed row simply
// rewrites failed_at to the latest timestamp; the worker stops polling
// the row as soon as the first MarkFailed lands so this should rarely
// fire twice.
func (s *CustomDomainStore) MarkFailed(ctx context.Context, id uuid.UUID, at time.Time, reason string) (management.Domain, error) {
	row := s.db.QueryRow(ctx, customDomainMarkFailedSQL, id, at, reason)
	return scanCustomDomainRowWithFailure(row)
}

// scanCustomDomainRow reads one row of the standard SELECT shape. ErrNoRows
// maps to management.ErrStoreNotFound so callers can errors.Is without
// importing pgx.
func scanCustomDomainRow(row pgx.Row) (management.Domain, error) {
	var (
		id, tenantID   [16]byte
		host, token    string
		verifiedAt     *time.Time
		verifiedDNSSEC bool
		pausedAt       *time.Time
		deletedAt      *time.Time
		dnsLogID       *[16]byte
		createdAt, upd time.Time
	)
	if err := row.Scan(&id, &tenantID, &host, &token, &verifiedAt, &verifiedDNSSEC, &pausedAt, &deletedAt, &dnsLogID, &createdAt, &upd); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return management.Domain{}, management.ErrStoreNotFound
		}
		return management.Domain{}, fmt.Errorf("custom_domain scan: %w", err)
	}
	d := management.Domain{
		ID:                 uuid.UUID(id),
		TenantID:           uuid.UUID(tenantID),
		Host:               host,
		VerificationToken:  token,
		VerifiedAt:         verifiedAt,
		VerifiedWithDNSSEC: verifiedDNSSEC,
		TLSPausedAt:        pausedAt,
		DeletedAt:          deletedAt,
		CreatedAt:          createdAt,
		UpdatedAt:          upd,
	}
	if dnsLogID != nil {
		uid := uuid.UUID(*dnsLogID)
		d.DNSResolutionLogID = &uid
	}
	return d, nil
}

// scanCustomDomainRowWithFailure reads the extended SELECT shape used by
// ListPendingVerification + MarkFailed (SIN-63080). The columns mirror
// the standard projection plus failed_at + failure_reason. Kept as a
// separate scanner so the existing fixtures around scanCustomDomainRow
// (custom_domain_store_test.go) stay untouched.
func scanCustomDomainRowWithFailure(row pgx.Row) (management.Domain, error) {
	var (
		id, tenantID   [16]byte
		host, token    string
		verifiedAt     *time.Time
		verifiedDNSSEC bool
		pausedAt       *time.Time
		deletedAt      *time.Time
		failedAt       *time.Time
		failureReason  *string
		dnsLogID       *[16]byte
		createdAt, upd time.Time
	)
	if err := row.Scan(&id, &tenantID, &host, &token, &verifiedAt, &verifiedDNSSEC, &pausedAt, &deletedAt, &failedAt, &failureReason, &dnsLogID, &createdAt, &upd); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return management.Domain{}, management.ErrStoreNotFound
		}
		return management.Domain{}, fmt.Errorf("custom_domain scan: %w", err)
	}
	d := management.Domain{
		ID:                 uuid.UUID(id),
		TenantID:           uuid.UUID(tenantID),
		Host:               host,
		VerificationToken:  token,
		VerifiedAt:         verifiedAt,
		VerifiedWithDNSSEC: verifiedDNSSEC,
		TLSPausedAt:        pausedAt,
		DeletedAt:          deletedAt,
		FailedAt:           failedAt,
		CreatedAt:          createdAt,
		UpdatedAt:          upd,
	}
	if failureReason != nil {
		d.FailureReason = *failureReason
	}
	if dnsLogID != nil {
		uid := uuid.UUID(*dnsLogID)
		d.DNSResolutionLogID = &uid
	}
	return d, nil
}

// Compile-time guard: the store satisfies management.Store.
var _ management.Store = (*CustomDomainStore)(nil)
