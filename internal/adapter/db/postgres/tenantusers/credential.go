package tenantusers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/tenantusers"
)

// compile-time assertion that Store also satisfies the credential-token port.
var _ domain.CredentialTokenRepository = (*Store)(nil)

// LookupToken resolves a still-valid credential token by hash within the
// tenant (SIN-66510 §1). It joins the token to its user so the caller gets
// the email for the ADR 0070 §5 identity rule. The row is returned ONLY when
// the token is unexpired and unconsumed AND the user is a non-master, active
// account — a deactivated user's invite can never be redeemed. Lookup is by
// token_sha256; the plaintext is never compared. Not-found / expired /
// consumed all collapse to domain.ErrTokenInvalid (no oracle).
func (s *Store) LookupToken(ctx context.Context, tenantID uuid.UUID, tokenHash []byte, now time.Time) (domain.Invite, error) {
	var inv domain.Invite
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var purpose string
		row := tx.QueryRow(ctx, `
			SELECT t.user_id, u.email, t.purpose
			FROM user_credential_tokens t
			JOIN users u ON u.id = t.user_id AND u.tenant_id = t.tenant_id
			WHERE t.token_sha256 = $1
			  AND t.tenant_id = $2
			  AND t.consumed_at IS NULL
			  AND t.expires_at > $3
			  AND u.is_master = false
			  AND u.deactivated_at IS NULL
		`, tokenHash, tenantID, now)
		if err := row.Scan(&inv.UserID, &inv.Email, &purpose); err != nil {
			return err
		}
		inv.Purpose = domain.Purpose(purpose)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Invite{}, domain.ErrTokenInvalid
	}
	if err != nil {
		return domain.Invite{}, fmt.Errorf("tenantusers/postgres: LookupToken: %w", err)
	}
	return inv, nil
}

// ConsumeToken atomically consumes the token and writes the user's new
// password hash inside ONE tenant-scoped transaction (SIN-66510 §1 — no
// TOCTOU / replay). The single-use gate is the `consumed_at IS NULL` guard on
// the UPDATE ... RETURNING: a row lock serializes concurrent consumers, so
// exactly one wins and any loser sees zero rows → domain.ErrTokenInvalid. The
// same statement re-checks `expires_at > now` so a token that lapsed between
// the GET-side lookup and this write cannot be redeemed.
func (s *Store) ConsumeToken(ctx context.Context, tenantID uuid.UUID, tokenHash []byte, now time.Time, newPasswordHash string) (uuid.UUID, error) {
	var userID uuid.UUID
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE user_credential_tokens
			SET consumed_at = $3
			WHERE token_sha256 = $1
			  AND tenant_id = $2
			  AND consumed_at IS NULL
			  AND expires_at > $3
			RETURNING user_id
		`, tokenHash, tenantID, now)
		if err := row.Scan(&userID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE users
			SET password_hash = $3
			WHERE id = $1
			  AND tenant_id = $2
			  AND is_master = false
			  AND deactivated_at IS NULL
		`, userID, tenantID, newPasswordHash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Token pointed at a user that is master / deactivated / gone.
			// Roll the whole tx back (the consumed_at write is discarded) so
			// the token is neither burned nor the password set.
			return domain.ErrTokenInvalid
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, domain.ErrTokenInvalid
	}
	if errors.Is(err, domain.ErrTokenInvalid) {
		return uuid.Nil, domain.ErrTokenInvalid
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenantusers/postgres: ConsumeToken: %w", err)
	}
	return userID, nil
}
