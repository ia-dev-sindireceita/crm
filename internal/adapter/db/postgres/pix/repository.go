package pix

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domainpix "github.com/pericles-luz/crm/internal/billing/pix"
)

// Repository implements pix.Repository against the pix_charges table
// (migration 0102). Writes route through WithMasterOps because
// migration 0102 only grants INSERT/UPDATE/DELETE to app_master_ops
// and installs the master_ops_audit trigger.
//
// Reads also route through WithMasterOps because the webhook reconciler
// does not own a tenant context — the payload arrives before the tenant
// is known. Tenant-scoped reads (e.g. for the billing UI) belong on a
// separate adapter built on the app_runtime pool and live outside this
// PR.
type Repository struct {
	masterPool postgresadapter.TxBeginner
	actorID    uuid.UUID
}

// Compile-time port assertion.
var _ domainpix.Repository = (*Repository)(nil)

// NewRepository wraps masterPool. actorID is the bot user-id used by
// every WithMasterOps call (audit + RLS bypass). uuid.Nil is rejected.
//
// masterPool is typed as postgresadapter.TxBeginner so production
// (which passes a *pgxpool.Pool) and tests (which inject in-process
// fakes) can both satisfy the constructor without an adapter shim.
func NewRepository(masterPool postgresadapter.TxBeginner, actorID uuid.UUID) (*Repository, error) {
	if masterPool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, postgresadapter.ErrZeroActor
	}
	return &Repository{masterPool: masterPool, actorID: actorID}, nil
}

const selectPixChargeBase = `
	SELECT id, tenant_id, invoice_id,
	       COALESCE(external_id, ''), qr_code, copy_paste,
	       status, paid_at, expires_at, created_at, updated_at
	  FROM pix_charges
`

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domainpix.PIXCharge, error) {
	if id == uuid.Nil {
		return nil, domainpix.ErrNotFound
	}
	var out *domainpix.PIXCharge
	err := postgresadapter.WithMasterOps(ctx, r.masterPool, r.actorID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, selectPixChargeBase+` WHERE id = $1`, id)
		c, scanErr := scanPixCharge(row)
		if scanErr != nil {
			return scanErr
		}
		out = c
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domainpix.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pix/postgres: GetByID: %w", err)
	}
	return out, nil
}

func (r *Repository) GetByExternalID(ctx context.Context, externalID string) (*domainpix.PIXCharge, error) {
	if externalID == "" {
		return nil, domainpix.ErrNotFound
	}
	var out *domainpix.PIXCharge
	err := postgresadapter.WithMasterOps(ctx, r.masterPool, r.actorID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, selectPixChargeBase+` WHERE external_id = $1`, externalID)
		c, scanErr := scanPixCharge(row)
		if scanErr != nil {
			return scanErr
		}
		out = c
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domainpix.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pix/postgres: GetByExternalID: %w", err)
	}
	return out, nil
}

const upsertPixCharge = `
	INSERT INTO pix_charges
	  (id, tenant_id, invoice_id, external_id, qr_code, copy_paste,
	   status, paid_at, expires_at, created_at, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	ON CONFLICT (id) DO UPDATE
	  SET external_id = EXCLUDED.external_id,
	      status      = EXCLUDED.status,
	      paid_at     = EXCLUDED.paid_at,
	      updated_at  = EXCLUDED.updated_at
`

// Save inserts a new row OR updates the mutable columns of an existing
// one (status, paid_at, external_id, updated_at). The immutable columns
// (tenant_id, invoice_id, qr_code, copy_paste, expires_at, created_at)
// are written on INSERT only; ON CONFLICT does not touch them.
//
// actorID is ignored here because the master_ops_audit trigger uses the
// GUC set by WithMasterOps. The signature keeps the port shape so a
// future adapter that runs outside WithMasterOps (e.g. a fake) can echo
// the value the caller passes.
func (r *Repository) Save(ctx context.Context, c *domainpix.PIXCharge, actorID uuid.UUID) error {
	if c == nil {
		return fmt.Errorf("pix/postgres: Save: nil charge")
	}
	useActor := actorID
	if useActor == uuid.Nil {
		useActor = r.actorID
	}
	if useActor == uuid.Nil {
		return postgresadapter.ErrZeroActor
	}
	err := postgresadapter.WithMasterOps(ctx, r.masterPool, useActor, func(tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx, upsertPixCharge,
			c.ID(),
			c.TenantID(),
			c.InvoiceID(),
			nullIfEmpty(c.ExternalID()),
			c.QRCode(),
			c.CopyPaste(),
			string(c.Status()),
			paidAtArg(c.PaidAt()),
			c.ExpiresAt(),
			c.CreatedAt(),
			c.UpdatedAt(),
		)
		return execErr
	})
	if err == nil {
		return nil
	}
	if isUniqueViolation(err, "pix_charges_external_id_uniq") {
		return domainpix.ErrExternalIDAlreadySet
	}
	if isCheckViolation(err, "pix_charges_paid_at_consistency") {
		return domainpix.ErrInvalidTransition
	}
	return fmt.Errorf("pix/postgres: Save: %w", err)
}

func (r *Repository) ListExpiredPending(ctx context.Context, before time.Time, limit int) ([]*domainpix.PIXCharge, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []*domainpix.PIXCharge
	err := postgresadapter.WithMasterOps(ctx, r.masterPool, r.actorID, func(tx pgx.Tx) error {
		rows, qErr := tx.Query(ctx, selectPixChargeBase+`
			 WHERE status = 'pending'
			   AND expires_at <= $1
			 ORDER BY expires_at ASC
			 LIMIT $2`, before, limit)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			c, scanErr := scanPixCharge(rows)
			if scanErr != nil {
				return scanErr
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pix/postgres: ListExpiredPending: %w", err)
	}
	return out, nil
}

// rowScanner is the minimum surface pgx.Row and pgx.Rows both
// satisfy. Lets scanPixCharge feed both QueryRow and Query result
// iteration.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanPixCharge(row rowScanner) (*domainpix.PIXCharge, error) {
	var (
		id, tenantID, invoiceID uuid.UUID
		externalID              string
		qrCode, copyPaste       string
		statusStr               string
		paidAt                  *time.Time
		expiresAt               time.Time
		createdAt, updatedAt    time.Time
	)
	if err := row.Scan(
		&id, &tenantID, &invoiceID,
		&externalID, &qrCode, &copyPaste,
		&statusStr, &paidAt, &expiresAt, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	status := domainpix.Status(statusStr)
	return domainpix.HydrateCharge(
		id, tenantID, invoiceID,
		externalID, qrCode, copyPaste,
		status,
		paidAt,
		expiresAt, createdAt, updatedAt,
	), nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func paidAtArg(p *time.Time) any {
	if p == nil {
		return nil
	}
	return *p
}

func isCheckViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != "23514" {
		return false
	}
	return pgErr.ConstraintName == constraint
}
