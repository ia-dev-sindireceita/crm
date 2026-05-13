package aesgcm

import (
	"github.com/pericles-luz/crm/internal/iam/password"
)

// RecoveryHasher wraps password.Argon2idHasher to satisfy the
// mfa.CodeHasher interface. The port drops the rehash signal —
// recovery codes are single-use so there is no persistent entry to
// rehash on read.
type RecoveryHasher struct {
	inner *password.Argon2idHasher
}

// NewRecoveryHasher returns a RecoveryHasher backed by the default
// Argon2id parameters.
func NewRecoveryHasher() *RecoveryHasher {
	return &RecoveryHasher{inner: password.Default()}
}

// Hash delegates to Argon2idHasher.Hash.
func (r *RecoveryHasher) Hash(plain string) (string, error) {
	return r.inner.Hash(plain)
}

// Verify delegates to Argon2idHasher.Verify, dropping the rehash
// signal. A Verify error from the hasher is returned as (false, err).
func (r *RecoveryHasher) Verify(stored, plain string) (bool, error) {
	ok, _, err := r.inner.Verify(stored, plain)
	return ok, err
}
