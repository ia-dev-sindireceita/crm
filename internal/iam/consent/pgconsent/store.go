// Package pgconsent is the pgx-backed Postgres adapter for
// consent.ConsentRegistry. It targets migration 0107's consent_record
// table; every query runs inside a WithTenant scope so the RLS
// policies the migration installs gate every read and write.
package pgconsent

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/iam/consent"
)

// Store implements consent.ConsentRegistry on top of the
// consent_record table. Construct via NewStore; the pool MUST be the
// app_runtime pool so the RLS policies on consent_record gate every
// query.
type Store struct {
	pool postgres.TxBeginner
}

var _ consent.ConsentRegistry = (*Store)(nil)

// NewStore wraps pool and returns a ready Store. A nil pool yields
// postgres.ErrNilPool so cmd/server fails fast on misconfiguration.
func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{pool: pool}, nil
}

const selectColumns = `id, tenant_id, subject_type, subject_id, purpose, version,
       granted, granted_at, revoked_at, revoke_reason,
       ip::text, user_agent`

// Record persists rec keyed by
// (tenant_id, subject_type, subject_id, purpose, version). ON CONFLICT
// DO NOTHING collapses duplicate calls into a single row; the adapter
// reports created=false when the conflict fires so the decorator can
// skip the audit emission.
//
// The persisted record is returned with the ID, GrantedAt, and
// Granted columns populated from the row that won the insert race
// (whether new or pre-existing).
func (s *Store) Record(ctx context.Context, rec consent.ConsentRecord) (consent.ConsentRecord, bool, error) {
	if err := validateRecord(rec); err != nil {
		return consent.ConsentRecord{}, false, fmt.Errorf("pgconsent: Record: %w", err)
	}

	var (
		persisted consent.ConsentRecord
		created   bool
	)
	err := postgres.WithTenant(ctx, s.pool, rec.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO consent_record
			  (tenant_id, subject_type, subject_id, purpose, version,
			   granted, ip, user_agent)
			VALUES ($1, $2, $3, $4, $5, true, $6, $7)
			ON CONFLICT (tenant_id, subject_type, subject_id, purpose, version)
			DO NOTHING
			RETURNING `+selectColumns,
			rec.TenantID,
			string(rec.Subject.Type),
			rec.Subject.ID,
			string(rec.Purpose),
			rec.Version,
			ipOrNil(rec.IP),
			nullableString(rec.UserAgent),
		)
		scanned, err := scanRecord(row)
		if err == nil {
			persisted = scanned
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Conflict path: row exists already, fetch the canonical
		// version so the decorator can return it to the caller.
		row = tx.QueryRow(ctx, `
			SELECT `+selectColumns+`
			  FROM consent_record
			 WHERE tenant_id    = $1
			   AND subject_type = $2
			   AND subject_id   = $3
			   AND purpose      = $4
			   AND version      = $5
		`,
			rec.TenantID,
			string(rec.Subject.Type),
			rec.Subject.ID,
			string(rec.Purpose),
			rec.Version,
		)
		scanned, err = scanRecord(row)
		if err != nil {
			return err
		}
		persisted = scanned
		created = false
		return nil
	})
	if err != nil {
		return consent.ConsentRecord{}, false, fmt.Errorf("pgconsent: Record: %w", err)
	}
	return persisted, created, nil
}

// Latest returns the row with the most recent granted_at for
// (tenant, subject, purpose), or (nil, nil) when none exist.
func (s *Store) Latest(ctx context.Context, tenant uuid.UUID, subject consent.Subject, purpose consent.Purpose) (*consent.ConsentRecord, error) {
	if err := validateLookup(tenant, subject, purpose); err != nil {
		return nil, fmt.Errorf("pgconsent: Latest: %w", err)
	}

	var out *consent.ConsentRecord
	err := postgres.WithTenant(ctx, s.pool, tenant, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+selectColumns+`
			  FROM consent_record
			 WHERE subject_type = $1
			   AND subject_id   = $2
			   AND purpose      = $3
			 ORDER BY granted_at DESC
			 LIMIT 1
		`,
			string(subject.Type),
			subject.ID,
			string(purpose),
		)
		rec, err := scanRecord(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = &rec
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("pgconsent: Latest: %w", err)
	}
	return out, nil
}

// History returns every row for (tenant, subject, purpose) ordered
// by granted_at DESC. An empty slice (not nil) means no rows match.
func (s *Store) History(ctx context.Context, tenant uuid.UUID, subject consent.Subject, purpose consent.Purpose) ([]consent.ConsentRecord, error) {
	if err := validateLookup(tenant, subject, purpose); err != nil {
		return nil, fmt.Errorf("pgconsent: History: %w", err)
	}

	out := make([]consent.ConsentRecord, 0)
	err := postgres.WithTenant(ctx, s.pool, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+selectColumns+`
			  FROM consent_record
			 WHERE subject_type = $1
			   AND subject_id   = $2
			   AND purpose      = $3
			 ORDER BY granted_at DESC
		`,
			string(subject.Type),
			subject.ID,
			string(purpose),
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			rec, err := scanRecord(rows)
			if err != nil {
				return err
			}
			out = append(out, rec)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pgconsent: History: %w", err)
	}
	return out, nil
}

// Revoke locates the most recent active grant for
// (q.TenantID, q.Subject, q.Purpose), flips granted=false, stamps
// revoked_at=now() and revoke_reason=q.Reason, and returns the
// updated row. ErrNoActiveGrant is returned when no active grant
// matches — the caller can present an idempotent "already revoked"
// UI rather than retry.
func (s *Store) Revoke(ctx context.Context, q consent.RevokeQuery) (consent.ConsentRecord, error) {
	if err := validateRevoke(q); err != nil {
		return consent.ConsentRecord{}, fmt.Errorf("pgconsent: Revoke: %w", err)
	}

	var updated consent.ConsentRecord
	err := postgres.WithTenant(ctx, s.pool, q.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			WITH target AS (
				SELECT id
				  FROM consent_record
				 WHERE subject_type = $1
				   AND subject_id   = $2
				   AND purpose      = $3
				   AND granted      = true
				   AND revoked_at   IS NULL
				 ORDER BY granted_at DESC
				 LIMIT 1
				 FOR UPDATE
			)
			UPDATE consent_record c
			   SET granted       = false,
			       revoked_at    = now(),
			       revoke_reason = $4
			  FROM target
			 WHERE c.id = target.id
			RETURNING c.id, c.tenant_id, c.subject_type, c.subject_id,
			          c.purpose, c.version, c.granted, c.granted_at,
			          c.revoked_at, c.revoke_reason,
			          c.ip::text, c.user_agent
		`,
			string(q.Subject.Type),
			q.Subject.ID,
			string(q.Purpose),
			q.Reason,
		)
		rec, err := scanRecord(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return consent.ErrNoActiveGrant
		}
		if err != nil {
			return err
		}
		updated = rec
		return nil
	})
	if errors.Is(err, consent.ErrNoActiveGrant) {
		return consent.ConsentRecord{}, err
	}
	if err != nil {
		return consent.ConsentRecord{}, fmt.Errorf("pgconsent: Revoke: %w", err)
	}
	return updated, nil
}

// rowScanner is the minimal Scan surface scanRecord needs; pgx.Row
// and pgx.Rows both satisfy it.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRecord decodes one consent_record row into a ConsentRecord.
// inet is cast to text on the SELECT side so the driver hands back
// a plain string the helper can parse with netip.ParseAddr; a NULL
// ip column scans to (*string)(nil) which becomes the zero
// netip.Addr in the result.
func scanRecord(r rowScanner) (consent.ConsentRecord, error) {
	var (
		rec           consent.ConsentRecord
		subjectType   string
		purpose       string
		revokedAt     *time.Time
		revokeReason  *string
		ipText        *string
		userAgentText *string
	)
	if err := r.Scan(
		&rec.ID,
		&rec.TenantID,
		&subjectType,
		&rec.Subject.ID,
		&purpose,
		&rec.Version,
		&rec.Granted,
		&rec.GrantedAt,
		&revokedAt,
		&revokeReason,
		&ipText,
		&userAgentText,
	); err != nil {
		return consent.ConsentRecord{}, err
	}
	rec.Subject.Type = consent.SubjectType(subjectType)
	rec.Purpose = consent.Purpose(purpose)
	if revokedAt != nil {
		t := *revokedAt
		rec.RevokedAt = &t
	}
	if revokeReason != nil {
		rec.RevokeReason = *revokeReason
	}
	if ipText != nil {
		trimmed := strings.TrimSpace(*ipText)
		// PostgreSQL's inet::text cast appends the mask suffix even
		// for a /32 host address (e.g. "198.51.100.7/32"). Strip the
		// suffix before parsing — the domain only stores host
		// addresses, not networks. A future use-case that needs the
		// network half can move to netip.Prefix on the Go side.
		if i := strings.IndexByte(trimmed, '/'); i >= 0 {
			trimmed = trimmed[:i]
		}
		if trimmed != "" {
			if addr, err := netip.ParseAddr(trimmed); err == nil {
				rec.IP = addr
			}
		}
	}
	if userAgentText != nil {
		rec.UserAgent = *userAgentText
	}
	return rec, nil
}

// validateRecord rejects ConsentRecord values that violate the
// boundary invariants before reaching the SQL layer. The CHECK
// constraints catch the same violations server-side; the adapter
// rejects earlier so callers see a typed sentinel.
func validateRecord(rec consent.ConsentRecord) error {
	if rec.TenantID == uuid.Nil {
		return consent.ErrInvalidTenant
	}
	if !rec.Subject.Type.IsValid() {
		return consent.ErrInvalidSubjectType
	}
	if strings.TrimSpace(rec.Subject.ID) == "" {
		return consent.ErrInvalidSubjectID
	}
	if !rec.Purpose.IsValid() {
		return consent.ErrInvalidPurpose
	}
	if strings.TrimSpace(rec.Version) == "" {
		return consent.ErrInvalidVersion
	}
	return nil
}

// validateRevoke rejects RevokeQuery values that violate the
// boundary invariants before reaching the SQL layer.
func validateRevoke(q consent.RevokeQuery) error {
	if q.TenantID == uuid.Nil {
		return consent.ErrInvalidTenant
	}
	if !q.Subject.Type.IsValid() {
		return consent.ErrInvalidSubjectType
	}
	if strings.TrimSpace(q.Subject.ID) == "" {
		return consent.ErrInvalidSubjectID
	}
	if !q.Purpose.IsValid() {
		return consent.ErrInvalidPurpose
	}
	if strings.TrimSpace(q.Reason) == "" {
		return consent.ErrInvalidRevokeReason
	}
	return nil
}

// validateLookup rejects Latest/History inputs before SQL.
func validateLookup(tenant uuid.UUID, subject consent.Subject, purpose consent.Purpose) error {
	if tenant == uuid.Nil {
		return consent.ErrInvalidTenant
	}
	if !subject.Type.IsValid() {
		return consent.ErrInvalidSubjectType
	}
	if strings.TrimSpace(subject.ID) == "" {
		return consent.ErrInvalidSubjectID
	}
	if !purpose.IsValid() {
		return consent.ErrInvalidPurpose
	}
	return nil
}

// ipOrNil renders rec.IP as a string for pgx; the zero address
// becomes nil so PostgreSQL stores NULL in the inet column.
func ipOrNil(addr netip.Addr) any {
	if !addr.IsValid() {
		return nil
	}
	return addr.String()
}

// nullableString collapses the empty string to nil so the SQL layer
// stores NULL rather than an empty user_agent string.
func nullableString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
