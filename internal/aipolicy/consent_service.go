package aipolicy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ConsentService implements the HasConsent / RecordConsent operations
// on top of a ConsentRepository. The service is the single place the
// SHA-256 of the payload preview is computed; callers pass the
// already-anonymized preview text and never the cleartext, so the
// cleartext PII never reaches the storage adapter.
//
// SIN-62928 / Fase 3 decisão #8. See ADR-0041 / ADR-0042 for the
// LGPD posture: a tenant cannot proceed to an LLM call until the
// operator has accepted the anonymized preview for the (tenant,
// scope_kind, scope_id) triple the call is about to act under.
type ConsentService struct {
	repo ConsentRepository
}

// NewConsentService wraps repo and returns a ready ConsentService.
// A nil repo is a programming bug; the constructor rejects it so the
// process fails fast at wiring time instead of panicking on the first
// HasConsent call.
func NewConsentService(repo ConsentRepository) (*ConsentService, error) {
	if repo == nil {
		return nil, ErrNilConsentRepository
	}
	return &ConsentService{repo: repo}, nil
}

// HasConsent reports whether a live consent row exists for scope AND
// the supplied (anonymizerVersion, promptVersion) match the values on
// the row. Either version mismatching is the cascade-on-bump trigger
// the gate relies on (AC #4 of SIN-62352): when the anonymizer rolls
// forward, or when the prompt template version changes, the next IA
// call falls through to a re-consent flow.
//
// Returns (false, nil) for a missing row and (false, nil) for a
// version mismatch. A non-nil error is reserved for transport /
// validation failures and is wrapped with context so callers can keep
// surfacing the typed sentinel via errors.Is.
func (s *ConsentService) HasConsent(
	ctx context.Context,
	scope ConsentScope,
	anonymizerVersion, promptVersion string,
) (bool, error) {
	if err := validateConsentScope(scope); err != nil {
		return false, fmt.Errorf("aipolicy: HasConsent: %w", err)
	}
	if strings.TrimSpace(anonymizerVersion) == "" {
		return false, fmt.Errorf("aipolicy: HasConsent: %w", ErrInvalidAnonymizerVersion)
	}
	if strings.TrimSpace(promptVersion) == "" {
		return false, fmt.Errorf("aipolicy: HasConsent: %w", ErrInvalidPromptVersion)
	}

	consent, ok, err := s.repo.Get(ctx, scope.TenantID, scope.Kind, scope.ID)
	if err != nil {
		return false, fmt.Errorf("aipolicy: HasConsent: %w", err)
	}
	if !ok {
		return false, nil
	}
	if consent.AnonymizerVersion != anonymizerVersion {
		return false, nil
	}
	if consent.PromptVersion != promptVersion {
		return false, nil
	}
	return true, nil
}

// RecordConsent stores the SHA-256 digest of payloadPreview for
// scope under (anonymizerVersion, promptVersion). The caller MUST
// pass a payload that has already been anonymized — the service does
// not anonymize on the caller's behalf so the same preview the
// operator just confirmed in the UI is what gets hashed.
//
// Idempotence: when the existing row already carries the same
// (PayloadHash, AnonymizerVersion, PromptVersion) the service returns
// nil without calling Upsert. A hash mismatch, an anonymizer-version
// bump, or a prompt-version bump all flow through to a single Upsert
// that updates the row in place (the UNIQUE constraint on
// (tenant_id, scope_kind, scope_id) keeps this to one row per scope).
//
// actorUserID is propagated to the row; pass nil when the actor is
// unknown (the column is ON DELETE SET NULL).
func (s *ConsentService) RecordConsent(
	ctx context.Context,
	scope ConsentScope,
	actorUserID *uuid.UUID,
	payloadPreview, anonymizerVersion, promptVersion string,
) error {
	if err := validateConsentScope(scope); err != nil {
		return fmt.Errorf("aipolicy: RecordConsent: %w", err)
	}
	if strings.TrimSpace(anonymizerVersion) == "" {
		return fmt.Errorf("aipolicy: RecordConsent: %w", ErrInvalidAnonymizerVersion)
	}
	if strings.TrimSpace(promptVersion) == "" {
		return fmt.Errorf("aipolicy: RecordConsent: %w", ErrInvalidPromptVersion)
	}

	hash := sha256.Sum256([]byte(payloadPreview))

	current, ok, err := s.repo.Get(ctx, scope.TenantID, scope.Kind, scope.ID)
	if err != nil {
		return fmt.Errorf("aipolicy: RecordConsent: %w", err)
	}
	if ok &&
		current.PayloadHash == hash &&
		current.AnonymizerVersion == anonymizerVersion &&
		current.PromptVersion == promptVersion {
		return nil
	}

	consent := Consent{
		TenantID:          scope.TenantID,
		ScopeKind:         scope.Kind,
		ScopeID:           scope.ID,
		ActorUserID:       actorUserID,
		PayloadHash:       hash,
		AnonymizerVersion: anonymizerVersion,
		PromptVersion:     promptVersion,
	}
	if err := s.repo.Upsert(ctx, consent); err != nil {
		return fmt.Errorf("aipolicy: RecordConsent: %w", err)
	}
	return nil
}

// validateConsentScope is the single shared boundary check for the
// service: the tenant must be non-zero, the kind must be one of the
// three allowed values, and the id must be non-blank. The adapter
// applies the same rules but the service rejects earlier so callers
// see a typed sentinel instead of a SQL error.
func validateConsentScope(s ConsentScope) error {
	if s.TenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	if !s.Kind.IsValid() {
		return ErrInvalidScopeType
	}
	if strings.TrimSpace(s.ID) == "" {
		return ErrInvalidScopeID
	}
	return nil
}
