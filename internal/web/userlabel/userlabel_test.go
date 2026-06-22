package userlabel_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/web/userlabel"
)

// fakeDirectory is an in-memory userlabel.Directory mirroring the
// inbox-usecase fakeDirectory test double (list_conversation_summaries_test.go):
// labels maps user id → label; ids absent from the map resolve to "no
// label", matching the adapter contract. A non-nil err short-circuits the
// lookup to exercise the degrade path.
type fakeDirectory struct {
	labels map[uuid.UUID]string
	err    error
	calls  int
	gotIDs []uuid.UUID
}

func (f *fakeDirectory) LabelsByID(_ context.Context, _ uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	f.calls++
	f.gotIDs = append([]uuid.UUID(nil), ids...)
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[uuid.UUID]string, len(ids))
	for _, id := range ids {
		if l, ok := f.labels[id]; ok {
			out[id] = l
		}
	}
	return out, nil
}

func TestResolve(t *testing.T) {
	t.Parallel()

	tenant := uuid.New()
	known := uuid.New()
	missing := uuid.New()

	tests := []struct {
		name   string
		dir    userlabel.Directory
		userID uuid.UUID
		want   string
	}{
		{
			name:   "resolved label",
			dir:    &fakeDirectory{labels: map[uuid.UUID]string{known: "agent"}},
			userID: known,
			want:   "agent",
		},
		{
			name:   "missing id falls back to Conta",
			dir:    &fakeDirectory{labels: map[uuid.UUID]string{known: "agent"}},
			userID: missing,
			want:   userlabel.Fallback,
		},
		{
			name:   "nil user falls back to Conta without touching the directory",
			dir:    &fakeDirectory{labels: map[uuid.UUID]string{known: "agent"}},
			userID: uuid.Nil,
			want:   userlabel.Fallback,
		},
		{
			name:   "nil directory falls back to Conta",
			dir:    nil,
			userID: known,
			want:   userlabel.Fallback,
		},
		{
			name:   "lookup error degrades to Conta",
			dir:    &fakeDirectory{err: errors.New("boom")},
			userID: known,
			want:   userlabel.Fallback,
		},
		{
			name:   "empty label falls back to Conta",
			dir:    &fakeDirectory{labels: map[uuid.UUID]string{known: ""}},
			userID: known,
			want:   userlabel.Fallback,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := userlabel.Resolve(context.Background(), tc.dir, tenant, tc.userID)
			if got != tc.want {
				t.Fatalf("Resolve(%s) = %q, want %q", tc.userID, got, tc.want)
			}
		})
	}
}

// TestResolve_NilUserSkipsLookup pins that the nil-user shortcut never
// hits the directory — the top bar must not pay a query for an
// unauthenticated render.
func TestResolve_NilUserSkipsLookup(t *testing.T) {
	t.Parallel()
	dir := &fakeDirectory{labels: map[uuid.UUID]string{}}
	if got := userlabel.Resolve(context.Background(), dir, uuid.New(), uuid.Nil); got != userlabel.Fallback {
		t.Fatalf("nil user -> %q, want %q", got, userlabel.Fallback)
	}
	if dir.calls != 0 {
		t.Fatalf("directory called %d times for nil user, want 0", dir.calls)
	}
}

// TestResolve_PassesSingleID pins that Resolve queries exactly the
// logged-in id (the port is batch-shaped but the top bar resolves one).
func TestResolve_PassesSingleID(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	dir := &fakeDirectory{labels: map[uuid.UUID]string{id: "agent"}}
	userlabel.Resolve(context.Background(), dir, uuid.New(), id)
	if len(dir.gotIDs) != 1 || dir.gotIDs[0] != id {
		t.Fatalf("LabelsByID got ids %v, want [%s]", dir.gotIDs, id)
	}
}
