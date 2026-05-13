package iam

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/password"
)

// UserPasswordWriter is the slice of the user-store the password-set
// flow needs — just enough to overwrite user.password_hash for a given
// (tenantID, userID). The postgres adapter is responsible for running
// the UPDATE inside WithTenant; the contract on the implementation is:
//
//   - The write MUST be its own transaction (separate from any login
//     read-side transaction) per ADR 0070 §3.
//   - A write to a missing or wrong-tenant row MUST return a non-nil
//     error so callers can distinguish "no-op" from "ok".
//
// The interface is deliberately narrow ("accept broad / return narrow"):
// the rest of the user lifecycle (create, list, lookup) belongs on
// other ports.
type UserPasswordWriter interface {
	UpdatePasswordHash(ctx context.Context, tenantID, userID uuid.UUID, encoded string) error
}

// ErrPasswordWriteUnavailable is what SetPassword returns when there is
// no PasswordWriter configured on the Service. Distinguishing this from
// a real DB error lets the boundary handler render a 5xx with a clearer
// reason.
var ErrPasswordWriteUnavailable = errors.New("iam: password writer not configured")

// SetPassword runs the ADR 0070 §5 policy check and, on pass, hashes the
// plaintext and persists the encoded form via PasswordWriter. The
// plaintext is never logged at any verbosity.
//
// pctx carries the per-request identity values (email, username, tenant
// name) that drive the §5 identity rule plus the optional pepper from
// §4. The boundary handler is responsible for filling pctx — SetPassword
// itself does not consult any store for identity values.
//
// Returns:
//
//   - *password.PolicyError when the policy rejects (caller renders a
//     localized message keyed by Reason).
//   - Any other error indicates an infra failure; callers should treat
//     it as 5xx-eligible.
func (s *Service) SetPassword(ctx context.Context, tenantID, userID uuid.UUID, plain string, pctx password.PolicyContext) error {
	if s.PasswordPolicy == nil || s.PasswordHasher == nil {
		return fmt.Errorf("iam: SetPassword requires PasswordPolicy and PasswordHasher")
	}
	if s.PasswordWriter == nil {
		return ErrPasswordWriteUnavailable
	}
	if err := s.PasswordPolicy.PolicyCheck(ctx, plain, pctx); err != nil {
		return err
	}
	encoded, err := s.PasswordHasher.Hash(plain)
	if err != nil {
		return fmt.Errorf("iam: hash password: %w", err)
	}
	if err := s.PasswordWriter.UpdatePasswordHash(ctx, tenantID, userID, encoded); err != nil {
		return fmt.Errorf("iam: update password hash: %w", err)
	}
	s.logger().InfoContext(ctx, "password: set",
		slog.String("tenant_id", tenantID.String()),
		slog.String("user_id", userID.String()),
	)
	return nil
}
