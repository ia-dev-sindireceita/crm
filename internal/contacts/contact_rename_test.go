package contacts

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestContact_Rename(t *testing.T) {
	pinned := time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)
	bumped := pinned.Add(time.Hour)

	tests := []struct {
		name      string
		startName string
		newName   string
		wantName  string
		wantErr   error
		wantBump  bool
	}{
		{name: "rename to new value", startName: "Alice", newName: "Alicia", wantName: "Alicia", wantBump: true},
		{name: "trims surrounding whitespace", startName: "Alice", newName: "  Bob  ", wantName: "Bob", wantBump: true},
		{name: "blank name rejected", startName: "Alice", newName: "   ", wantName: "Alice", wantErr: ErrEmptyDisplayName},
		{name: "empty name rejected", startName: "Alice", newName: "", wantName: "Alice", wantErr: ErrEmptyDisplayName},
		{name: "no-op rename keeps timestamp", startName: "Alice", newName: "Alice", wantName: "Alice", wantBump: false},
		{name: "no-op after trim keeps timestamp", startName: "Alice", newName: "  Alice  ", wantName: "Alice", wantBump: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Hydrate(uuid.New(), uuid.New(), tt.startName, nil, pinned, pinned)
			now = func() time.Time { return bumped }
			defer func() { now = func() time.Time { return time.Now().UTC() } }()

			err := c.Rename(tt.newName)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Rename err = %v, want %v", err, tt.wantErr)
			}
			if c.DisplayName != tt.wantName {
				t.Errorf("DisplayName = %q, want %q", c.DisplayName, tt.wantName)
			}
			if tt.wantBump {
				if !c.UpdatedAt.Equal(bumped) {
					t.Errorf("UpdatedAt = %v, want bumped %v", c.UpdatedAt, bumped)
				}
			} else {
				if !c.UpdatedAt.Equal(pinned) {
					t.Errorf("UpdatedAt = %v, want unchanged %v", c.UpdatedAt, pinned)
				}
			}
		})
	}
}
