package rules

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestInMemoryRepository_Admin_CreateGetUpdateDelete(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	repo := NewInMemoryRepository()
	ctx := context.Background()

	r, err := NewRule(uuid.Nil, tenant, "webchat", nil, "rule-1",
		TriggerTypeMessageContains, map[string]any{"phrase": "x"},
		ActionTypeMoveToStage, map[string]any{"stage_key": "novo"},
		true, fixedNow)
	if err != nil {
		t.Fatalf("NewRule: %v", err)
	}
	if err := repo.Create(ctx, r); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.Get(ctx, tenant, r.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "rule-1" {
		t.Fatalf("Get returned wrong row: %+v", got)
	}

	r.Name = "rule-1-renamed"
	r.Enabled = false
	if err := repo.Update(ctx, r); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = repo.Get(ctx, tenant, r.ID)
	if got.Name != "rule-1-renamed" || got.Enabled {
		t.Fatalf("Update did not stick: %+v", got)
	}
	if !got.UpdatedAt.After(r.CreatedAt) {
		t.Fatalf("Update should advance UpdatedAt, got %v vs created %v", got.UpdatedAt, r.CreatedAt)
	}

	if err := repo.SetEnabled(ctx, tenant, r.ID, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	got, _ = repo.Get(ctx, tenant, r.ID)
	if !got.Enabled {
		t.Fatal("SetEnabled(true) did not flip the flag")
	}

	if err := repo.Delete(ctx, tenant, r.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, tenant, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: want ErrNotFound, got %v", err)
	}
}

func TestInMemoryRepository_Admin_ListAllOrdersByCascade(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	team := uuid.New()
	repo := NewInMemoryRepository()
	ctx := context.Background()

	mustCreate := func(channel string, teamID *uuid.UUID, name string, enabled bool) {
		t.Helper()
		r, err := NewRule(uuid.Nil, tenant, channel, teamID, name,
			TriggerTypeMessageContains, map[string]any{"phrase": "x"},
			ActionTypeMoveToStage, map[string]any{"stage_key": "novo"},
			enabled, fixedNow)
		if err != nil {
			t.Fatalf("NewRule(%s): %v", name, err)
		}
		if err := repo.Create(ctx, r); err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
	}
	mustCreate("", nil, "tenant-default", true)
	mustCreate("", &team, "team-rule", false) // disabled, still must surface
	mustCreate("webchat", nil, "channel-rule", true)

	all, err := repo.ListAll(ctx, tenant)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 rules, got %d (%+v)", len(all), all)
	}
	wantOrder := []string{"channel-rule", "team-rule", "tenant-default"}
	for i, w := range wantOrder {
		if all[i].Name != w {
			t.Fatalf("rule[%d]: want %q, got %q", i, w, all[i].Name)
		}
	}
}

func TestInMemoryRepository_Admin_CrossTenantInvisible(t *testing.T) {
	t.Parallel()
	tenantA := uuid.New()
	tenantB := uuid.New()
	repo := NewInMemoryRepository()
	ctx := context.Background()

	rA, _ := NewRule(uuid.Nil, tenantA, "", nil, "A",
		TriggerTypeMessageContains, map[string]any{"phrase": "x"},
		ActionTypeMoveToStage, map[string]any{"stage_key": "novo"},
		true, fixedNow)
	_ = repo.Create(ctx, rA)

	if _, err := repo.Get(ctx, tenantB, rA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get under tenantB must return ErrNotFound, got %v", err)
	}
	if err := repo.Delete(ctx, tenantB, rA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete under tenantB must return ErrNotFound, got %v", err)
	}
	if err := repo.SetEnabled(ctx, tenantB, rA.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetEnabled under tenantB must return ErrNotFound, got %v", err)
	}
}

func TestInMemoryRepository_Admin_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	repo := NewInMemoryRepository()
	ctx := context.Background()
	if _, err := repo.ListAll(ctx, uuid.Nil); !errors.Is(err, ErrInvalidTenant) {
		t.Fatalf("ListAll(nil): want ErrInvalidTenant, got %v", err)
	}
	if _, err := repo.Get(ctx, uuid.Nil, uuid.New()); !errors.Is(err, ErrInvalidTenant) {
		t.Fatalf("Get(nil): want ErrInvalidTenant, got %v", err)
	}
	if err := repo.Create(ctx, Rule{}); !errors.Is(err, ErrInvalidTenant) {
		t.Fatalf("Create(nil tenant): want ErrInvalidTenant, got %v", err)
	}
	if err := repo.Update(ctx, Rule{}); !errors.Is(err, ErrInvalidTenant) {
		t.Fatalf("Update(nil tenant): want ErrInvalidTenant, got %v", err)
	}
	if err := repo.SetEnabled(ctx, uuid.Nil, uuid.New(), true); !errors.Is(err, ErrInvalidTenant) {
		t.Fatalf("SetEnabled(nil): want ErrInvalidTenant, got %v", err)
	}
	if err := repo.Delete(ctx, uuid.Nil, uuid.New()); !errors.Is(err, ErrInvalidTenant) {
		t.Fatalf("Delete(nil): want ErrInvalidTenant, got %v", err)
	}
}

func TestInMemoryRepository_Admin_UpdateUnknownReturnsNotFound(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	repo := NewInMemoryRepository()
	r, _ := NewRule(uuid.New(), tenant, "", nil, "n",
		TriggerTypeMessageContains, map[string]any{"phrase": "x"},
		ActionTypeMoveToStage, map[string]any{"stage_key": "novo"},
		true, fixedNow)
	if err := repo.Update(context.Background(), r); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update on unknown: want ErrNotFound, got %v", err)
	}
}
