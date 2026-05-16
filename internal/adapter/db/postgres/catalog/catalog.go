// Package catalog (adapter) implements catalog.ProductRepository and
// catalog.ArgumentRepository against Postgres. Reads route through
// the runtime pool inside WithTenant so RLS gates tenant visibility.
// Writes route through the master pool inside WithMasterOps so the
// audit trigger fires on every mutation.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/catalog"
)

var (
	_ catalog.ProductRepository  = (*Store)(nil)
	_ catalog.ArgumentRepository = (*Store)(nil)
)

// Store implements the two catalog ports against Postgres.
//
// runtimePool connects as app_runtime (RLS applies; BYPASSRLS=false).
// It is used for tenant-scoped reads on product and product_argument.
//
// masterPool connects as app_master_ops (BYPASSRLS=true; audit
// trigger fires). It is used for all writes.
type Store struct {
	runtimePool *pgxpool.Pool
	masterPool  *pgxpool.Pool
}

// New constructs a Store. Both pools must be non-nil.
func New(runtimePool, masterPool *pgxpool.Pool) (*Store, error) {
	if runtimePool == nil || masterPool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	return &Store{runtimePool: runtimePool, masterPool: masterPool}, nil
}

// ---------------------------------------------------------------------------
// ProductRepository
// ---------------------------------------------------------------------------

// GetByID returns the product for (tenantID, productID), or
// catalog.ErrNotFound.
func (s *Store) GetByID(ctx context.Context, tenantID, productID uuid.UUID) (*catalog.Product, error) {
	if tenantID == uuid.Nil {
		return nil, catalog.ErrZeroTenant
	}
	var p *catalog.Product
	err := postgresadapter.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		got, err := scanProduct(tx.QueryRow(ctx,
			`SELECT id, tenant_id, name, description, price_cents, tags,
			        created_at, updated_at
			   FROM product
			  WHERE id = $1 AND tenant_id = $2`, productID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return catalog.ErrNotFound
			}
			return fmt.Errorf("catalog/postgres: get product: %w", err)
		}
		p = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ListByTenant returns all products for tenantID ordered by created_at ASC.
func (s *Store) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*catalog.Product, error) {
	if tenantID == uuid.Nil {
		return nil, catalog.ErrZeroTenant
	}
	var products []*catalog.Product
	err := postgresadapter.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, name, description, price_cents, tags,
			        created_at, updated_at
			   FROM product
			  WHERE tenant_id = $1
			  ORDER BY created_at ASC`, tenantID)
		if err != nil {
			return fmt.Errorf("catalog/postgres: list products: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProduct(rows)
			if err != nil {
				return fmt.Errorf("catalog/postgres: scan product: %w", err)
			}
			products = append(products, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return products, nil
}

// SaveProduct inserts or updates the product row. The upsert key is
// the product's primary key (id); mutable fields are refreshed on
// conflict. actorID is recorded in the master_ops audit trail.
func (s *Store) SaveProduct(ctx context.Context, p *catalog.Product, actorID uuid.UUID) error {
	if p == nil {
		return fmt.Errorf("catalog/postgres: product is nil")
	}
	return postgresadapter.WithMasterOps(ctx, s.masterPool, actorID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO product
			  (id, tenant_id, name, description, price_cents, tags,
			   created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (id) DO UPDATE SET
			  name        = EXCLUDED.name,
			  description = EXCLUDED.description,
			  price_cents = EXCLUDED.price_cents,
			  tags        = EXCLUDED.tags,
			  updated_at  = EXCLUDED.updated_at`,
			p.ID(), p.TenantID(), p.Name(), p.Description(), p.PriceCents(),
			p.Tags(), p.CreatedAt(), p.UpdatedAt(),
		)
		if err != nil {
			return fmt.Errorf("catalog/postgres: save product: %w", err)
		}
		return nil
	})
}

// DeleteProduct removes the product. ON DELETE CASCADE on
// product_argument's FK clears arguments alongside it. Returns
// catalog.ErrNotFound when no row matched.
func (s *Store) DeleteProduct(ctx context.Context, tenantID, productID, actorID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return catalog.ErrZeroTenant
	}
	return postgresadapter.WithMasterOps(ctx, s.masterPool, actorID, func(tx pgx.Tx) error {
		// master_ops bypasses RLS, so an explicit tenant predicate
		// stops a typo in productID from deleting another tenant's
		// row that happens to share a UUID prefix in a future
		// migration. Defense in depth on top of the RLS layer.
		tag, err := tx.Exec(ctx,
			`DELETE FROM product WHERE id = $1 AND tenant_id = $2`,
			productID, tenantID)
		if err != nil {
			return fmt.Errorf("catalog/postgres: delete product: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return catalog.ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// ArgumentRepository
// ---------------------------------------------------------------------------

// ListByProduct returns all arguments attached to productID for
// tenantID, ordered by (scope_type, scope_id, created_at).
func (s *Store) ListByProduct(ctx context.Context, tenantID, productID uuid.UUID) ([]*catalog.ProductArgument, error) {
	if tenantID == uuid.Nil {
		return nil, catalog.ErrZeroTenant
	}
	var args []*catalog.ProductArgument
	err := postgresadapter.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, product_id, scope_type, scope_id,
			        argument_text, created_at, updated_at
			   FROM product_argument
			  WHERE tenant_id = $1 AND product_id = $2
			  ORDER BY scope_type, scope_id, created_at`, tenantID, productID)
		if err != nil {
			return fmt.Errorf("catalog/postgres: list arguments: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanArgument(rows)
			if err != nil {
				return fmt.Errorf("catalog/postgres: scan argument: %w", err)
			}
			args = append(args, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return args, nil
}

// SaveArgument inserts or updates the argument row. The upsert key is
// the argument's primary key (id); argument_text and updated_at are
// refreshed on conflict. A unique-violation on
// product_argument_product_scope_uniq is translated to
// catalog.ErrDuplicateArgument.
func (s *Store) SaveArgument(ctx context.Context, a *catalog.ProductArgument, actorID uuid.UUID) error {
	if a == nil {
		return fmt.Errorf("catalog/postgres: argument is nil")
	}
	return postgresadapter.WithMasterOps(ctx, s.masterPool, actorID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO product_argument
			  (id, tenant_id, product_id, scope_type, scope_id,
			   argument_text, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (id) DO UPDATE SET
			  argument_text = EXCLUDED.argument_text,
			  updated_at    = EXCLUDED.updated_at`,
			a.ID(), a.TenantID(), a.ProductID(),
			string(a.Anchor().Type), a.Anchor().ID,
			a.Text(), a.CreatedAt(), a.UpdatedAt(),
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return catalog.ErrDuplicateArgument
			}
			return fmt.Errorf("catalog/postgres: save argument: %w", err)
		}
		return nil
	})
}

// DeleteArgument removes the argument matching (tenantID,
// argumentID). Returns catalog.ErrNotFound when no row matched.
func (s *Store) DeleteArgument(ctx context.Context, tenantID, argumentID, actorID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return catalog.ErrZeroTenant
	}
	return postgresadapter.WithMasterOps(ctx, s.masterPool, actorID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM product_argument WHERE id = $1 AND tenant_id = $2`,
			argumentID, tenantID)
		if err != nil {
			return fmt.Errorf("catalog/postgres: delete argument: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return catalog.ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// row scanners
// ---------------------------------------------------------------------------

func scanProduct(row pgx.Row) (*catalog.Product, error) {
	var (
		id          uuid.UUID
		tenantID    uuid.UUID
		name        string
		description string
		priceCents  int
		tags        []string
		createdAt   time.Time
		updatedAt   time.Time
	)
	if err := row.Scan(&id, &tenantID, &name, &description, &priceCents, &tags,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return catalog.HydrateProduct(id, tenantID, name, description, priceCents,
		tags, createdAt, updatedAt), nil
}

func scanArgument(row pgx.Row) (*catalog.ProductArgument, error) {
	var (
		id        uuid.UUID
		tenantID  uuid.UUID
		productID uuid.UUID
		scopeType string
		scopeID   string
		text      string
		createdAt time.Time
		updatedAt time.Time
	)
	if err := row.Scan(&id, &tenantID, &productID, &scopeType, &scopeID,
		&text, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	anchor := catalog.ScopeAnchor{
		Type: catalog.ScopeType(scopeType),
		ID:   scopeID,
	}
	return catalog.HydrateProductArgument(id, tenantID, productID, anchor,
		text, createdAt, updatedAt), nil
}
