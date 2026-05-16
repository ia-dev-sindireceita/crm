package catalog_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/catalog"
)

// stubLister returns canned arguments — no DB, no SQL. The resolver
// only consumes ListByProduct so this stub is enough.
type stubLister struct {
	args []*catalog.ProductArgument
	err  error
}

func (s *stubLister) ListByProduct(
	_ context.Context,
	_, _ uuid.UUID,
) ([]*catalog.ProductArgument, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([]*catalog.ProductArgument, len(s.args))
	copy(out, s.args)
	return out, nil
}

func arg(t *testing.T, productID uuid.UUID, st catalog.ScopeType, id, text string) *catalog.ProductArgument {
	t.Helper()
	a, err := catalog.NewProductArgument(uuid.New(), productID,
		catalog.ScopeAnchor{Type: st, ID: id}, text,
		time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewProductArgument(%s/%s): %v", st, id, err)
	}
	return a
}

// TestResolver_AcceptanceCriterion proves the AC from SIN-62902:
// given 3 arguments (1 tenant, 1 team, 1 channel), ResolveArguments
// returns them ordered channel > team > tenant.
func TestResolver_AcceptanceCriterion(t *testing.T) {
	productID := uuid.New()
	teamID := uuid.New().String()
	channelKey := "whatsapp"

	tenantArg := arg(t, productID, catalog.ScopeTenant, uuid.NewString(), "tenant-pitch")
	teamArg := arg(t, productID, catalog.ScopeTeam, teamID, "team-pitch")
	channelArg := arg(t, productID, catalog.ScopeChannel, channelKey, "channel-pitch")

	r := catalog.NewResolver(&stubLister{args: []*catalog.ProductArgument{
		// Deliberately out-of-order — the resolver must sort.
		tenantArg, teamArg, channelArg,
	}})

	got, err := r.ResolveArguments(context.Background(), uuid.New(), productID,
		catalog.Scope{TeamID: teamID, ChannelID: channelKey})
	if err != nil {
		t.Fatalf("ResolveArguments: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d args, want 3", len(got))
	}
	if got[0].Text() != "channel-pitch" {
		t.Errorf("got[0] = %q, want channel-pitch", got[0].Text())
	}
	if got[1].Text() != "team-pitch" {
		t.Errorf("got[1] = %q, want team-pitch", got[1].Text())
	}
	if got[2].Text() != "tenant-pitch" {
		t.Errorf("got[2] = %q, want tenant-pitch", got[2].Text())
	}
}

func TestResolver_ScopeFiltering(t *testing.T) {
	productID := uuid.New()
	team1 := uuid.New().String()
	team2 := uuid.New().String()

	tenantArg := arg(t, productID, catalog.ScopeTenant, uuid.NewString(), "tenant")
	team1Arg := arg(t, productID, catalog.ScopeTeam, team1, "team1")
	team2Arg := arg(t, productID, catalog.ScopeTeam, team2, "team2")
	channelWA := arg(t, productID, catalog.ScopeChannel, "whatsapp", "wa")
	channelIG := arg(t, productID, catalog.ScopeChannel, "instagram", "ig")

	cases := []struct {
		name     string
		scope    catalog.Scope
		wantText []string
	}{
		{
			"channel-only",
			catalog.Scope{ChannelID: "whatsapp"},
			[]string{"wa", "tenant"},
		},
		{
			"team-only",
			catalog.Scope{TeamID: team1},
			[]string{"team1", "tenant"},
		},
		{
			"team+channel match both",
			catalog.Scope{TeamID: team1, ChannelID: "whatsapp"},
			[]string{"wa", "team1", "tenant"},
		},
		{
			"channel mismatch",
			catalog.Scope{ChannelID: "telegram"},
			[]string{"tenant"},
		},
		{
			"empty scope falls back to tenant catch-all",
			catalog.Scope{},
			[]string{"tenant"},
		},
	}

	r := catalog.NewResolver(&stubLister{args: []*catalog.ProductArgument{
		tenantArg, team1Arg, team2Arg, channelWA, channelIG,
	}})

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.ResolveArguments(context.Background(), uuid.New(), productID, tc.scope)
			if err != nil {
				t.Fatalf("ResolveArguments: %v", err)
			}
			gotText := make([]string, len(got))
			for i, a := range got {
				gotText[i] = a.Text()
			}
			if len(gotText) != len(tc.wantText) {
				t.Fatalf("got %v, want %v", gotText, tc.wantText)
			}
			for i := range gotText {
				if gotText[i] != tc.wantText[i] {
					t.Errorf("got[%d] = %q, want %q (full: got=%v want=%v)",
						i, gotText[i], tc.wantText[i], gotText, tc.wantText)
				}
			}
		})
	}
}

func TestResolver_Errors(t *testing.T) {
	productID := uuid.New()
	tenantID := uuid.New()

	t.Run("nil tenant", func(t *testing.T) {
		r := catalog.NewResolver(&stubLister{})
		_, err := r.ResolveArguments(context.Background(), uuid.Nil, productID, catalog.Scope{})
		if !errors.Is(err, catalog.ErrZeroTenant) {
			t.Errorf("err = %v, want ErrZeroTenant", err)
		}
	})
	t.Run("nil product", func(t *testing.T) {
		r := catalog.NewResolver(&stubLister{})
		_, err := r.ResolveArguments(context.Background(), tenantID, uuid.Nil, catalog.Scope{})
		if !errors.Is(err, catalog.ErrInvalidArgument) {
			t.Errorf("err = %v, want ErrInvalidArgument", err)
		}
	})
	t.Run("repository error propagates", func(t *testing.T) {
		stub := &stubLister{err: errors.New("boom")}
		r := catalog.NewResolver(stub)
		_, err := r.ResolveArguments(context.Background(), tenantID, productID, catalog.Scope{})
		if err == nil || err.Error() != "boom" {
			t.Errorf("err = %v, want repository error", err)
		}
	})
}

func TestResolver_SkipsNilEntries(t *testing.T) {
	productID := uuid.New()
	a := arg(t, productID, catalog.ScopeTenant, "t", "x")
	r := catalog.NewResolver(&stubLister{args: []*catalog.ProductArgument{nil, a, nil}})
	got, err := r.ResolveArguments(context.Background(), uuid.New(), productID, catalog.Scope{})
	if err != nil {
		t.Fatalf("ResolveArguments: %v", err)
	}
	if len(got) != 1 || got[0] != a {
		t.Errorf("got %v, want exactly [a]", got)
	}
}
