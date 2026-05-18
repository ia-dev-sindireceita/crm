package main

// SIN-62965 — wire-up tests for the DunningTick worker. Exercise the
// env-driven gating and fail-soft branches without needing a real
// Postgres connection.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func stubDunningDial(pool *pgxpool.Pool, err error, called *int) dunningTickDial {
	return func(_ context.Context, _ string) (*pgxpool.Pool, error) {
		if called != nil {
			*called++
		}
		return pool, err
	}
}

func TestBuildDunningTickWiring_Disabled(t *testing.T) {
	validActor := uuid.New().String()
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"unset feature flag", map[string]string{}},
		{"feature flag != 1", map[string]string{envDunningTickEnabled: "true"}},
		{"missing master DSN", map[string]string{envDunningTickEnabled: "1"}},
		{
			"missing actor",
			map[string]string{
				envDunningTickEnabled: "1",
				envMasterOpsDSN:       "postgres://master:pw@localhost/crm",
			},
		},
		{
			"invalid actor uuid",
			map[string]string{
				envDunningTickEnabled: "1",
				envMasterOpsDSN:       "postgres://master:pw@localhost/crm",
				envDunningTickActor:   "not-a-uuid",
			},
		},
		{
			"nil actor uuid",
			map[string]string{
				envDunningTickEnabled: "1",
				envMasterOpsDSN:       "postgres://master:pw@localhost/crm",
				envDunningTickActor:   "00000000-0000-0000-0000-000000000000",
			},
		},
		{
			"invalid tick-every",
			map[string]string{
				envDunningTickEnabled: "1",
				envMasterOpsDSN:       "postgres://master:pw@localhost/crm",
				envDunningTickActor:   validActor,
				envDunningTickEvery:   "not-a-duration",
			},
		},
		{
			"invalid batch size",
			map[string]string{
				envDunningTickEnabled:   "1",
				envMasterOpsDSN:         "postgres://master:pw@localhost/crm",
				envDunningTickActor:     validActor,
				envDunningTickBatchSize: "abc",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dialed int
			got := buildDunningTickWiringWithDeps(
				context.Background(),
				envFromMap(tc.env),
				stubDunningDial(nil, errors.New("should not be called"), &dialed),
			)
			if got != nil {
				t.Errorf("expected nil wiring, got %+v", got)
			}
		})
	}
}

func TestBuildDunningTickWiring_DialError(t *testing.T) {
	env := map[string]string{
		envDunningTickEnabled: "1",
		envMasterOpsDSN:       "postgres://master:pw@localhost/crm",
		envDunningTickActor:   uuid.New().String(),
	}
	var dialed int
	got := buildDunningTickWiringWithDeps(
		context.Background(),
		envFromMap(env),
		stubDunningDial(nil, errors.New("connection refused"), &dialed),
	)
	if got != nil {
		t.Fatalf("expected nil wiring on dial error, got %+v", got)
	}
	if dialed == 0 {
		t.Error("dial was not invoked")
	}
}
