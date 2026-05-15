package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
)

func TestChannelAssociationLookup_HappyPath(t *testing.T) {
	t.Parallel()
	want := [16]byte{0x77, 0x88}
	conn := stubConn{
		queryRow: func(sql string, args ...any) pgx.Row {
			if len(args) != 2 || args[0] != "whatsapp" || args[1] != "phone-123" {
				t.Errorf("unexpected args: %v", args)
			}
			return fakeRow{values: []any{want}}
		},
	}
	got, err := pgstore.NewChannelAssociationLookup(conn).Resolve(context.Background(), "whatsapp", "phone-123")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestChannelAssociationLookup_MissReturnsSentinel(t *testing.T) {
	t.Parallel()
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := pgstore.NewChannelAssociationLookup(conn).Resolve(context.Background(), "whatsapp", "missing")
	if !errors.Is(err, pgstore.ErrAssociationUnknown) {
		t.Fatalf("expected ErrAssociationUnknown, got %v", err)
	}
}

func TestChannelAssociationLookup_OtherErrorWrapped(t *testing.T) {
	t.Parallel()
	other := errors.New("connection refused")
	conn := stubConn{
		queryRow: func(string, ...any) pgx.Row {
			return fakeRow{err: other}
		},
	}
	_, err := pgstore.NewChannelAssociationLookup(conn).Resolve(context.Background(), "whatsapp", "x")
	if err == nil || errors.Is(err, pgstore.ErrAssociationUnknown) {
		t.Fatalf("expected wrapped other-error, got %v", err)
	}
	if !errors.Is(err, other) {
		t.Fatalf("expected to wrap original error, got %v", err)
	}
}
