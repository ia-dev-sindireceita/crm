package webchat

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// EnvFeatureFlag implements FeatureFlag from two env vars:
// FEATURE_WEBCHAT_ENABLED and FEATURE_WEBCHAT_TENANTS (CSV of tenant UUIDs).
// An empty allowlist means "all tenants". globalOn=false means "all off".
type EnvFeatureFlag struct {
	globalOn bool
	allowed  map[uuid.UUID]struct{}
}

// NewEnvFeatureFlag parses the feature flag env vars. Invalid UUIDs in the
// allowlist are silently skipped.
func NewEnvFeatureFlag(getenv func(string) string) *EnvFeatureFlag {
	if getenv == nil {
		return &EnvFeatureFlag{}
	}
	on := strings.TrimSpace(getenv(EnvEnabled)) == "1"
	allow := map[uuid.UUID]struct{}{}
	for _, raw := range strings.Split(getenv(EnvTenantAllow), ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if id, err := uuid.Parse(s); err == nil {
			allow[id] = struct{}{}
		}
	}
	return &EnvFeatureFlag{globalOn: on, allowed: allow}
}

// Enabled implements FeatureFlag.
func (f *EnvFeatureFlag) Enabled(_ context.Context, tenantID uuid.UUID) (bool, error) {
	if f == nil || !f.globalOn {
		return false, nil
	}
	if len(f.allowed) == 0 {
		return true, nil
	}
	_, ok := f.allowed[tenantID]
	return ok, nil
}
