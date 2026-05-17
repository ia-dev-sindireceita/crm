package rules

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestInMemoryRepository_ListEffectiveForChannel_HonoursAllThreeScopes
// is a focused smoke for the fake — it proves the in-memory port
// implementation respects the contract documented on
// [RuleRepository.ListEffectiveForChannel]: channel match is exact,
// team scope only fires when teamID is non-nil, tenant scope always
// fires, disabled rows never surface.
func TestInMemoryRepository_ListEffectiveForChannel_HonoursAllThreeScopes(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	team := uuid.New()
	otherTeam := uuid.New()

	repo := NewInMemoryRepository()
	repo.Seed(
		rule(1, tenant, withChannel("webchat"), withAction("channel-hit")),
		rule(2, tenant, withChannel("whatsapp"), withAction("wrong-channel")),
		rule(3, tenant, withTeam(team), withAction("team-hit")),
		rule(4, tenant, withTeam(otherTeam), withAction("wrong-team")),
		rule(5, tenant, withAction("tenant-default")),
		rule(6, tenant, withChannel("webchat"), withDisabled(), withAction("disabled-channel")),
	)

	got, err := repo.ListEffectiveForChannel(context.Background(), tenant, "webchat", team)
	if err != nil {
		t.Fatalf("ListEffectiveForChannel: %v", err)
	}

	// Expect rules 1 (channel hit), 3 (team hit), 5 (tenant default) in cascade order.
	want := []string{"channel-hit", "team-hit", "tenant-default"}
	if len(got) != len(want) {
		t.Fatalf("want %d rules, got %d (%+v)", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i].ActionConfig["stage_key"] != w {
			t.Fatalf("rule[%d]: want %q, got %q (full=%+v)",
				i, w, got[i].ActionConfig["stage_key"], got[i])
		}
	}
}

func TestInMemoryRepository_ListEffectiveForChannel_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	repo := NewInMemoryRepository()
	_, err := repo.ListEffectiveForChannel(context.Background(), uuid.Nil, "webchat", uuid.Nil)
	if err != ErrInvalidTenant {
		t.Fatalf("want ErrInvalidTenant, got %v", err)
	}
}

// TestInMemoryRepository_ConcurrentSeedAndList — make sure the mutex
// actually protects the slice under concurrent access.
func TestInMemoryRepository_ConcurrentSeedAndList(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	repo := NewInMemoryRepository()

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			repo.Seed(rule(byte(i%200), tenant, withAction("x")))
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		_, err := repo.ListEffectiveForChannel(ctx, tenant, "webchat", uuid.Nil)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
	}
	<-done
}
