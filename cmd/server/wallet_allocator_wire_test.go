package main

// SIN-62881 — wire-up tests for the WalletAllocator. Exercise the
// env-driven gating and fail-soft branches without dialing real
// Postgres or NATS. The dialer + nats connector are injected via
// the *WithDeps overload.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	natsgo "github.com/nats-io/nats.go"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
)

func wallocEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func stubWAllocDial(runtime, master *pgxpool.Pool, err error, called *bool) walletAllocatorDial {
	return func(_ context.Context, _, _ string) (*pgxpool.Pool, *pgxpool.Pool, error) {
		if called != nil {
			*called = true
		}
		return runtime, master, err
	}
}

func stubWAllocNATS(js natsgo.JetStreamContext, drain func(), err error, called *bool) walletAllocatorNATSConnect {
	return func(_ context.Context, _ natsadapter.SDKConfig) (natsgo.JetStreamContext, func(), error) {
		if called != nil {
			*called = true
		}
		return js, drain, err
	}
}

func TestBuildWalletAllocatorWiring_Disabled(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"unset feature flag", map[string]string{}},
		{"feature flag != 1", map[string]string{envWalletAllocatorEnabled: "true"}},
		{
			"missing runtime DSN",
			map[string]string{envWalletAllocatorEnabled: "1"},
		},
		{
			"missing master DSN",
			map[string]string{
				envWalletAllocatorEnabled: "1",
				envRuntimeDSN:             "postgres://app:pw@localhost/crm",
			},
		},
		{
			"missing actor",
			map[string]string{
				envWalletAllocatorEnabled: "1",
				envRuntimeDSN:             "postgres://app:pw@localhost/crm",
				"MASTER_OPS_DATABASE_URL": "postgres://master:pw@localhost/crm",
			},
		},
		{
			"invalid actor uuid",
			map[string]string{
				envWalletAllocatorEnabled: "1",
				envRuntimeDSN:             "postgres://app:pw@localhost/crm",
				"MASTER_OPS_DATABASE_URL": "postgres://master:pw@localhost/crm",
				envWalletAllocatorActor:   "not-a-uuid",
			},
		},
		{
			"nil actor uuid",
			map[string]string{
				envWalletAllocatorEnabled: "1",
				envRuntimeDSN:             "postgres://app:pw@localhost/crm",
				"MASTER_OPS_DATABASE_URL": "postgres://master:pw@localhost/crm",
				envWalletAllocatorActor:   uuid.Nil.String(),
			},
		},
		{
			"missing NATS URL",
			map[string]string{
				envWalletAllocatorEnabled: "1",
				envRuntimeDSN:             "postgres://app:pw@localhost/crm",
				"MASTER_OPS_DATABASE_URL": "postgres://master:pw@localhost/crm",
				envWalletAllocatorActor:   uuid.New().String(),
			},
		},
		{
			"invalid ack wait",
			map[string]string{
				envWalletAllocatorEnabled: "1",
				envRuntimeDSN:             "postgres://app:pw@localhost/crm",
				"MASTER_OPS_DATABASE_URL": "postgres://master:pw@localhost/crm",
				envWalletAllocatorActor:   uuid.New().String(),
				envNATSURL:                "tls://nats:4222",
				envWalletAllocatorAckWait: "not-a-duration",
			},
		},
		{
			"non-positive ack wait",
			map[string]string{
				envWalletAllocatorEnabled: "1",
				envRuntimeDSN:             "postgres://app:pw@localhost/crm",
				"MASTER_OPS_DATABASE_URL": "postgres://master:pw@localhost/crm",
				envWalletAllocatorActor:   uuid.New().String(),
				envNATSURL:                "tls://nats:4222",
				envWalletAllocatorAckWait: "-5s",
			},
		},
		{
			"invalid max deliver",
			map[string]string{
				envWalletAllocatorEnabled:    "1",
				envRuntimeDSN:                "postgres://app:pw@localhost/crm",
				"MASTER_OPS_DATABASE_URL":    "postgres://master:pw@localhost/crm",
				envWalletAllocatorActor:      uuid.New().String(),
				envNATSURL:                   "tls://nats:4222",
				envWalletAllocatorMaxDeliver: "abc",
			},
		},
		{
			"max deliver too small",
			map[string]string{
				envWalletAllocatorEnabled:    "1",
				envRuntimeDSN:                "postgres://app:pw@localhost/crm",
				"MASTER_OPS_DATABASE_URL":    "postgres://master:pw@localhost/crm",
				envWalletAllocatorActor:      uuid.New().String(),
				envNATSURL:                   "tls://nats:4222",
				envWalletAllocatorMaxDeliver: "1",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dialCalled := false
			natsCalled := false
			w := buildWalletAllocatorWiringWithDeps(
				context.Background(),
				wallocEnv(tc.env),
				stubWAllocDial(nil, nil, nil, &dialCalled),
				stubWAllocNATS(nil, nil, nil, &natsCalled),
			)
			if w != nil {
				t.Fatalf("wiring expected nil; got %+v", w)
			}
			// Disabled branches must short-circuit before dialing
			// real resources.
			if dialCalled {
				t.Error("dialer should not be invoked when env is invalid")
			}
			if natsCalled {
				t.Error("nats connect should not be invoked when env is invalid")
			}
		})
	}
}

func TestBuildWalletAllocatorWiring_DialError(t *testing.T) {
	dialCalled := false
	natsCalled := false
	env := map[string]string{
		envWalletAllocatorEnabled: "1",
		envRuntimeDSN:             "postgres://app:pw@localhost/crm",
		"MASTER_OPS_DATABASE_URL": "postgres://master:pw@localhost/crm",
		envWalletAllocatorActor:   uuid.New().String(),
		envNATSURL:                "tls://nats:4222",
	}
	w := buildWalletAllocatorWiringWithDeps(
		context.Background(),
		wallocEnv(env),
		stubWAllocDial(nil, nil, errors.New("pg down"), &dialCalled),
		stubWAllocNATS(nil, nil, nil, &natsCalled),
	)
	if w != nil {
		t.Fatalf("wiring expected nil on dial error; got %+v", w)
	}
	if !dialCalled {
		t.Error("dialer should have been invoked")
	}
	if natsCalled {
		t.Error("nats connect should not be invoked after dial error")
	}
}

func TestTruthyEnv(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"no":    false,
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"True":  true,
		"yes":   true,
		"on":    true,
	}
	for in, want := range cases {
		if got := truthyEnv(in); got != want {
			t.Errorf("truthyEnv(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestEnvOr(t *testing.T) {
	get := wallocEnv(map[string]string{"FOO": "bar"})
	if got := envOr(get, "FOO", "fallback"); got != "bar" {
		t.Errorf("envOr(FOO) = %q, want %q", got, "bar")
	}
	if got := envOr(get, "MISSING", "fallback"); got != "fallback" {
		t.Errorf("envOr(MISSING) = %q, want %q", got, "fallback")
	}
}
