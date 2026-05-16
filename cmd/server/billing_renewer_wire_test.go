package main

// SIN-62879 — wire-up tests for the BillingRenewer. These exercise the
// env-driven gating and fail-soft branches without needing a real
// Postgres connection or a NATS server.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// envFromMap returns a getenv-shaped closure backed by a map.
func envFromMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// stubDial returns the supplied pool/err regardless of dsn. It records
// whether it was invoked so tests can confirm the wire reached the dial
// step.
func stubDial(pool *pgxpool.Pool, err error, called *bool) billingRenewerDial {
	return func(_ context.Context, _ string) (*pgxpool.Pool, error) {
		if called != nil {
			*called = true
		}
		return pool, err
	}
}

func TestBuildBillingRenewerWiring_Disabled(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"unset feature flag", map[string]string{}},
		{"feature flag != 1", map[string]string{envBillingRenewerEnabled: "true"}},
		{
			"missing master DSN",
			map[string]string{envBillingRenewerEnabled: "1"},
		},
		{
			"missing actor",
			map[string]string{
				envBillingRenewerEnabled: "1",
				envMasterOpsDSN:          "postgres://master:pw@localhost/crm",
			},
		},
		{
			"invalid actor uuid",
			map[string]string{
				envBillingRenewerEnabled: "1",
				envMasterOpsDSN:          "postgres://master:pw@localhost/crm",
				envBillingRenewerActor:   "not-a-uuid",
			},
		},
		{
			"nil actor uuid",
			map[string]string{
				envBillingRenewerEnabled: "1",
				envMasterOpsDSN:          "postgres://master:pw@localhost/crm",
				envBillingRenewerActor:   uuid.Nil.String(),
			},
		},
		{
			"invalid tick duration",
			map[string]string{
				envBillingRenewerEnabled:   "1",
				envMasterOpsDSN:            "postgres://master:pw@localhost/crm",
				envBillingRenewerActor:     uuid.New().String(),
				envBillingRenewerTickEvery: "not-a-duration",
			},
		},
		{
			"non-positive tick duration",
			map[string]string{
				envBillingRenewerEnabled:   "1",
				envMasterOpsDSN:            "postgres://master:pw@localhost/crm",
				envBillingRenewerActor:     uuid.New().String(),
				envBillingRenewerTickEvery: "-1s",
			},
		},
		{
			"invalid batch size",
			map[string]string{
				envBillingRenewerEnabled:   "1",
				envMasterOpsDSN:            "postgres://master:pw@localhost/crm",
				envBillingRenewerActor:     uuid.New().String(),
				envBillingRenewerBatchSize: "abc",
			},
		},
		{
			"non-positive batch size",
			map[string]string{
				envBillingRenewerEnabled:   "1",
				envMasterOpsDSN:            "postgres://master:pw@localhost/crm",
				envBillingRenewerActor:     uuid.New().String(),
				envBillingRenewerBatchSize: "0",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			wiring := buildBillingRenewerWiringWithDeps(
				context.Background(),
				envFromMap(tc.env),
				stubDial(nil, nil, &called),
			)
			if wiring != nil {
				t.Errorf("wiring = %+v, want nil", wiring)
			}
			// The gate must short-circuit before the dial call.
			if called {
				t.Error("dial was invoked despite gating; expected early return")
			}
		})
	}
}

func TestBuildBillingRenewerWiring_DialError(t *testing.T) {
	env := map[string]string{
		envBillingRenewerEnabled: "1",
		envMasterOpsDSN:          "postgres://master:pw@localhost/crm",
		envBillingRenewerActor:   uuid.New().String(),
	}
	var called bool
	dial := stubDial(nil, errors.New("connection refused"), &called)

	wiring := buildBillingRenewerWiringWithDeps(context.Background(), envFromMap(env), dial)
	if wiring != nil {
		t.Errorf("wiring = %+v, want nil on dial error", wiring)
	}
	if !called {
		t.Error("dial was not invoked; expected the gate to reach dial step")
	}
}

func TestBillingNoopPublisher_PublishIsLossless(t *testing.T) {
	p := newBillingNoopPublisher()
	if err := p.Publish(context.Background(), "subscription.renewed", "msg-id", []byte("payload")); err != nil {
		t.Errorf("noop publish returned %v, want nil", err)
	}
}

// We deliberately do NOT test the happy path here. Successful wiring
// requires a real *pgxpool.Pool to call NewRenewerStore against, which
// in turn requires a live Postgres — that belongs in the
// internal/adapter/db/postgres/billing_renewer_adapter_test.go suite.
// Keeping this file pool-free preserves the cmd/server boot speed and
// avoids cross-package test fixtures.
