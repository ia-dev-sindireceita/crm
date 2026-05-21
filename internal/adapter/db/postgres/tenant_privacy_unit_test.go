package postgres_test

// SIN-63191 / Fase 6 PR4 — unit tests for the new LoadPrivacySettings
// adapter method. Drives the error paths (zero id, no rows, transient
// failure) and the happy path through a scanning stub so the coverage
// stays above 85% without spinning Postgres for this package.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// fillRow scans the six privacy-settings columns into the destination
// pointers. The shape matches tenantPrivacySettingsSQL — when that
// query is reshaped both adapter and stub move together.
type fillRow struct {
	dpoName   string
	dpoEmail  string
	version   string
	url       string
	markdown  string
	updatedAt *time.Time
}

func (r fillRow) Scan(dst ...any) error {
	if got := len(dst); got != 6 {
		return errors.New("scan dst count mismatch")
	}
	*(dst[0].(*string)) = r.dpoName
	*(dst[1].(*string)) = r.dpoEmail
	*(dst[2].(*string)) = r.version
	*(dst[3].(*string)) = r.url
	*(dst[4].(*string)) = r.markdown
	*(dst[5].(**time.Time)) = r.updatedAt
	return nil
}

func TestLoadPrivacySettings_HappyPath(t *testing.T) {
	t.Parallel()
	updated := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: fillRow{
		dpoName:   "Marina Soares",
		dpoEmail:  "dpo@acme.example",
		version:   "2026.05",
		url:       "https://acme.example/privacy.pdf",
		markdown:  "# Política",
		updatedAt: &updated,
	}})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := r.LoadPrivacySettings(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if settings.DPOName != "Marina Soares" {
		t.Errorf("DPOName = %q", settings.DPOName)
	}
	if settings.DPOEmail != "dpo@acme.example" {
		t.Errorf("DPOEmail = %q", settings.DPOEmail)
	}
	if settings.PrivacyPolicyVersion != "2026.05" {
		t.Errorf("PrivacyPolicyVersion = %q", settings.PrivacyPolicyVersion)
	}
	if settings.PrivacyPolicyURL != "https://acme.example/privacy.pdf" {
		t.Errorf("PrivacyPolicyURL = %q", settings.PrivacyPolicyURL)
	}
	if !strings.Contains(settings.PrivacyPolicyMarkdown, "Política") {
		t.Errorf("PrivacyPolicyMarkdown = %q", settings.PrivacyPolicyMarkdown)
	}
	if settings.PrivacyPolicyUpdated == nil || !settings.PrivacyPolicyUpdated.Equal(updated) {
		t.Errorf("PrivacyPolicyUpdated = %v; want %v", settings.PrivacyPolicyUpdated, updated)
	}
}

func TestLoadPrivacySettings_ZeroIDFails(t *testing.T) {
	t.Parallel()
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: stubRow{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.LoadPrivacySettings(context.Background(), uuid.Nil)
	if !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Fatalf("err = %v; want ErrTenantNotFound", err)
	}
}

func TestLoadPrivacySettings_NoRowsMapsToNotFound(t *testing.T) {
	t.Parallel()
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: stubRow{err: pgx.ErrNoRows}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.LoadPrivacySettings(context.Background(), uuid.New())
	if !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Fatalf("err = %v; want ErrTenantNotFound", err)
	}
}

func TestLoadPrivacySettings_TransientErrorWraps(t *testing.T) {
	t.Parallel()
	transient := errors.New("connection reset by peer")
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: stubRow{err: transient}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.LoadPrivacySettings(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, transient) {
		t.Errorf("err = %v; want wraps %v", err, transient)
	}
	if !strings.Contains(err.Error(), "privacy settings") {
		t.Errorf("err = %q; want context prefix", err.Error())
	}
}
