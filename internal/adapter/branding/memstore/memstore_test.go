package memstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/branding/memstore"
	"github.com/pericles-luz/crm/internal/branding"
)

func samplePalette() branding.Palette {
	return branding.Palette{
		Primary:       branding.RGB{R: 0x1f, G: 0x29, B: 0x37},
		Secondary:     branding.RGB{R: 0x37, G: 0x41, B: 0x51},
		Accent:        branding.RGB{R: 0x2d, G: 0x9c, B: 0xdb},
		Foreground:    branding.RGB{R: 0x0f, G: 0x11, B: 0x15},
		Background:    branding.RGB{R: 0xff, G: 0xff, B: 0xff},
		TextOnPrimary: branding.RGB{R: 0xff, G: 0xff, B: 0xff},
		Source:        branding.PaletteSourceManual,
	}
}

func TestStore_GetEmptyReturnsNotFound(t *testing.T) {
	t.Parallel()
	s := memstore.New()
	_, err := s.GetByTenantID(context.Background(), uuid.New())
	if !errors.Is(err, branding.ErrPaletteNotFound) {
		t.Fatalf("err=%v, want ErrPaletteNotFound", err)
	}
}

func TestStore_SetThenGetRoundtrips(t *testing.T) {
	t.Parallel()
	s := memstore.New()
	tid := uuid.New()
	want := samplePalette()
	if err := s.SetForTenant(context.Background(), tid, want); err != nil {
		t.Fatalf("SetForTenant: %v", err)
	}
	got, err := s.GetByTenantID(context.Background(), tid)
	if err != nil {
		t.Fatalf("GetByTenantID: %v", err)
	}
	if got != want {
		t.Fatalf("got=%+v, want=%+v", got, want)
	}
}

func TestStore_DeleteRemovesEntry(t *testing.T) {
	t.Parallel()
	s := memstore.New()
	tid := uuid.New()
	_ = s.SetForTenant(context.Background(), tid, samplePalette())
	if err := s.DeleteForTenant(context.Background(), tid); err != nil {
		t.Fatalf("DeleteForTenant: %v", err)
	}
	_, err := s.GetByTenantID(context.Background(), tid)
	if !errors.Is(err, branding.ErrPaletteNotFound) {
		t.Fatalf("after delete err=%v, want ErrPaletteNotFound", err)
	}
}

func TestStore_DeleteMissingIsSuccess(t *testing.T) {
	t.Parallel()
	s := memstore.New()
	if err := s.DeleteForTenant(context.Background(), uuid.New()); err != nil {
		t.Fatalf("DeleteForTenant on absent tenant: %v", err)
	}
}

func TestStore_TenantsAreIsolated(t *testing.T) {
	t.Parallel()
	s := memstore.New()
	a, b := uuid.New(), uuid.New()
	pal := samplePalette()
	_ = s.SetForTenant(context.Background(), a, pal)
	_, err := s.GetByTenantID(context.Background(), b)
	if !errors.Is(err, branding.ErrPaletteNotFound) {
		t.Fatalf("tenant b err=%v, want ErrPaletteNotFound", err)
	}
}

func TestStore_HonoursCanceledContext(t *testing.T) {
	t.Parallel()
	s := memstore.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetByTenantID(ctx, uuid.New()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get err=%v, want context.Canceled", err)
	}
	if err := s.SetForTenant(ctx, uuid.New(), samplePalette()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Set err=%v, want context.Canceled", err)
	}
	if err := s.DeleteForTenant(ctx, uuid.New()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete err=%v, want context.Canceled", err)
	}
}

func TestStore_ConcurrentReadsAndWrites(t *testing.T) {
	t.Parallel()
	s := memstore.New()
	tid := uuid.New()
	pal := samplePalette()
	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = s.SetForTenant(context.Background(), tid, pal)
		}()
		go func() {
			defer wg.Done()
			_, _ = s.GetByTenantID(context.Background(), tid)
		}()
	}
	wg.Wait()
}
