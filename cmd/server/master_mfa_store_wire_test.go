package main

// SIN-65222 (Child A) — composition-root tests for the master MFA
// store-stack glue. The handlers, postgres adapters, and mfa.Service
// are covered exhaustively in their own packages; these tests pin the
// wire-level behaviour: fail-soft on every missing/invalid input, the
// all-nil noop contract, the happy-path full assembly, the master seed
// cipher env parsing, and the master (nil-tenant) audit logger.
//
// Reuses envFunc / b64Key32 / stubSplit / recordingSplit declared in
// usermfa_wire_test.go (same package main).

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
)

// stubMasterLogin satisfies mastermfa.MasterLoginFunc. It is never
// invoked in these wire-level tests — its presence (non-nil) is what
// buildMasterMFAStack checks.
func stubMasterLogin(context.Context, string, string, string, net.IP, string, string) (iam.Session, error) {
	return iam.Session{}, errors.New("stub")
}

func TestNoopMasterMFAStack_AllNil(t *testing.T) {
	t.Parallel()
	s := noopMasterMFAStack()
	if s.Sessions != nil || s.HTTPSession != nil || s.Enroller != nil ||
		s.Verifier != nil || s.Consumer != nil || s.Regenerator != nil ||
		s.Enrollment != nil || s.Directory != nil || s.Failures != nil ||
		s.Invalidator != nil || s.Alerter != nil || s.Login != nil {
		t.Fatalf("noopMasterMFAStack must be all-nil, got %#v", s)
	}
}

func TestBuildMasterMFAStack_MissingInputs_ReturnsNoop(t *testing.T) {
	t.Parallel()
	validActor := uuid.New().String()
	goodKey := b64Key32(t)
	fullEnv := map[string]string{
		envMasterOpsActorID: validActor,
		envMasterMFASeedKey: goodKey,
	}

	cases := []struct {
		name   string
		pool   *pgxpool.Pool
		login  mastermfa.MasterLoginFunc
		writer audit.SplitLogger
		env    map[string]string
	}{
		{name: "pool nil", pool: nil, login: stubMasterLogin, writer: stubSplit{}, env: fullEnv},
		{name: "login nil", pool: &pgxpool.Pool{}, login: nil, writer: stubSplit{}, env: fullEnv},
		{name: "writer nil", pool: &pgxpool.Pool{}, login: stubMasterLogin, writer: nil, env: fullEnv},
		{name: "actor unset", pool: &pgxpool.Pool{}, login: stubMasterLogin, writer: stubSplit{}, env: map[string]string{envMasterMFASeedKey: goodKey}},
		{name: "actor invalid", pool: &pgxpool.Pool{}, login: stubMasterLogin, writer: stubSplit{}, env: map[string]string{envMasterOpsActorID: "not-a-uuid", envMasterMFASeedKey: goodKey}},
		{name: "actor nil-uuid", pool: &pgxpool.Pool{}, login: stubMasterLogin, writer: stubSplit{}, env: map[string]string{envMasterOpsActorID: uuid.Nil.String(), envMasterMFASeedKey: goodKey}},
		{name: "seed key unset", pool: &pgxpool.Pool{}, login: stubMasterLogin, writer: stubSplit{}, env: map[string]string{envMasterOpsActorID: validActor}},
		{name: "seed key malformed", pool: &pgxpool.Pool{}, login: stubMasterLogin, writer: stubSplit{}, env: map[string]string{envMasterOpsActorID: validActor, envMasterMFASeedKey: "not!base64!!"}},
		{name: "seed key wrong length", pool: &pgxpool.Pool{}, login: stubMasterLogin, writer: stubSplit{}, env: map[string]string{envMasterOpsActorID: validActor, envMasterMFASeedKey: base64.StdEncoding.EncodeToString(make([]byte, 16))}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := buildMasterMFAStack(context.Background(), tc.pool, tc.login, tc.writer, envFunc(tc.env))
			if s.Sessions != nil || s.HTTPSession != nil || s.Enroller != nil ||
				s.Verifier != nil || s.Directory != nil || s.Alerter != nil || s.Login != nil {
				t.Fatalf("expected noop stack for %q, got %#v", tc.name, s)
			}
		})
	}
}

func TestBuildMasterMFAStack_AllInputsPresent_FullyAssembled(t *testing.T) {
	t.Parallel()
	// Constructors only nil-check pool + actorID; they do not Exec until
	// a handler runs. A non-nil &pgxpool.Pool{} + valid actor + valid
	// 32-byte seed key therefore yields a fully-populated stack without
	// a live DB (rule 5: real adapter, no mock, no query at construction).
	env := envFunc(map[string]string{
		envMasterOpsActorID: uuid.New().String(),
		envMasterMFASeedKey: b64Key32(t),
	})
	s := buildMasterMFAStack(context.Background(), &pgxpool.Pool{}, stubMasterLogin, stubSplit{}, env)

	if s.Sessions == nil {
		t.Error("Sessions nil")
	}
	if s.HTTPSession == nil {
		t.Error("HTTPSession nil")
	}
	if s.Enroller == nil || s.Verifier == nil || s.Consumer == nil || s.Regenerator == nil {
		t.Error("mfa.Service-backed ports must be non-nil")
	}
	if s.Enrollment == nil {
		t.Error("Enrollment nil")
	}
	if s.Directory == nil {
		t.Error("Directory nil")
	}
	if s.Failures == nil {
		t.Error("Failures nil")
	}
	if s.Invalidator == nil {
		t.Error("Invalidator nil")
	}
	if s.Alerter == nil {
		t.Error("Alerter nil")
	}
	if s.Login == nil {
		t.Error("Login nil")
	}

	// Enroller/Verifier/Consumer/Regenerator are the SAME *mfa.Service.
	if any(s.Enroller) != any(s.Verifier) || any(s.Verifier) != any(s.Consumer) || any(s.Consumer) != any(s.Regenerator) {
		t.Error("expected one shared *mfa.Service backing all four handler ports")
	}
	// Invalidator is the same HTTPSession pointer.
	if inv, ok := s.Invalidator.(*mastermfa.HTTPSession); !ok || inv != s.HTTPSession {
		t.Error("Invalidator must be the same *mastermfa.HTTPSession instance")
	}
}

func TestBuildMasterMFAStack_RespectsIssuerOverride(t *testing.T) {
	t.Parallel()
	env := envFunc(map[string]string{
		envMasterOpsActorID: uuid.New().String(),
		envMasterMFASeedKey: b64Key32(t),
		envMasterMFAIssuer:  "ACME-Master",
	})
	s := buildMasterMFAStack(context.Background(), &pgxpool.Pool{}, stubMasterLogin, stubSplit{}, env)
	if s.Verifier == nil {
		t.Fatalf("expected active stack with issuer override")
	}
}

func TestBuildMasterMFASeedCipher_Cases(t *testing.T) {
	t.Parallel()
	good := b64Key32(t)
	cases := []struct {
		name    string
		env     string
		wantErr bool
	}{
		{name: "unset", env: "", wantErr: true},
		{name: "malformed base64", env: "@@@", wantErr: true},
		{name: "wrong length (16 bytes)", env: base64.StdEncoding.EncodeToString(make([]byte, 16)), wantErr: true},
		{name: "valid 32-byte key", env: good, wantErr: false},
		{name: "padded whitespace tolerated", env: "  " + good + "  ", wantErr: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := buildMasterMFASeedCipher(envFunc(map[string]string{envMasterMFASeedKey: tc.env}))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestMasterMFAAuditLogger_WritesNilTenantContext(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	cases := []struct {
		name      string
		call      func(l *masterMFAAuditLogger) error
		wantEvent audit.SecurityEvent
	}{
		{name: "enrolled", call: func(l *masterMFAAuditLogger) error { return l.LogEnrolled(context.Background(), userID) }, wantEvent: audit.SecurityEvent2FAEnroll},
		{name: "verified", call: func(l *masterMFAAuditLogger) error { return l.LogVerified(context.Background(), userID) }, wantEvent: audit.SecurityEvent2FAVerify},
		{name: "recovery used", call: func(l *masterMFAAuditLogger) error { return l.LogRecoveryUsed(context.Background(), userID) }, wantEvent: audit.SecurityEvent2FARecoveryUsed},
		{name: "recovery regenerated", call: func(l *masterMFAAuditLogger) error { return l.LogRecoveryRegenerated(context.Background(), userID) }, wantEvent: audit.SecurityEvent2FARecoveryRegenerated},
		{name: "mfa required", call: func(l *masterMFAAuditLogger) error {
			return l.LogMFARequired(context.Background(), userID, "/m/grants", "missing_session")
		}, wantEvent: audit.SecurityEvent2FARequired},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := &recordingSplit{}
			l := newMasterMFAAuditLogger(w)
			if err := tc.call(l); err != nil {
				t.Fatalf("call: %v", err)
			}
			if !w.called {
				t.Fatalf("expected WriteSecurity to be invoked")
			}
			if w.last.Event != tc.wantEvent {
				t.Errorf("event=%q want %q", w.last.Event, tc.wantEvent)
			}
			if w.last.TenantID != nil {
				t.Errorf("master events must carry a nil TenantID, got %v", *w.last.TenantID)
			}
			if w.last.ActorUserID != userID {
				t.Errorf("ActorUserID=%s want %s", w.last.ActorUserID, userID)
			}
		})
	}
}

func TestMasterMFAAuditLogger_MFARequiredTargetFields(t *testing.T) {
	t.Parallel()
	w := &recordingSplit{}
	l := newMasterMFAAuditLogger(w)
	if err := l.LogMFARequired(context.Background(), uuid.New(), "/m/grants", "missing_session"); err != nil {
		t.Fatalf("LogMFARequired: %v", err)
	}
	if got := w.last.Target["route"]; got != "/m/grants" {
		t.Errorf("target route=%v want /m/grants", got)
	}
	if got := w.last.Target["reason"]; got != "missing_session" {
		t.Errorf("target reason=%v want missing_session", got)
	}
}
