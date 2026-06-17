package contacts

import "testing"

func TestListFilter_Normalized(t *testing.T) {
	tests := []struct {
		name       string
		in         ListFilter
		wantQuery  string
		wantLimit  int
		wantOffset int
	}{
		{
			name:       "zero limit defaults",
			in:         ListFilter{Query: "alice", Limit: 0, Offset: 0},
			wantQuery:  "alice",
			wantLimit:  DefaultListLimit,
			wantOffset: 0,
		},
		{
			name:       "negative limit defaults",
			in:         ListFilter{Limit: -5},
			wantQuery:  "",
			wantLimit:  DefaultListLimit,
			wantOffset: 0,
		},
		{
			name:       "over-cap limit clamps",
			in:         ListFilter{Limit: MaxListLimit + 1000},
			wantLimit:  MaxListLimit,
			wantOffset: 0,
		},
		{
			name:       "negative offset floored",
			in:         ListFilter{Limit: 10, Offset: -3},
			wantLimit:  10,
			wantOffset: 0,
		},
		{
			name:       "query trimmed",
			in:         ListFilter{Query: "  bob  ", Limit: 10},
			wantQuery:  "bob",
			wantLimit:  10,
			wantOffset: 0,
		},
		{
			name:       "in-range values preserved",
			in:         ListFilter{Query: "x", Limit: 25, Offset: 50},
			wantQuery:  "x",
			wantLimit:  25,
			wantOffset: 50,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.Normalized()
			if got.Query != tt.wantQuery {
				t.Errorf("Query = %q, want %q", got.Query, tt.wantQuery)
			}
			if got.Limit != tt.wantLimit {
				t.Errorf("Limit = %d, want %d", got.Limit, tt.wantLimit)
			}
			if got.Offset != tt.wantOffset {
				t.Errorf("Offset = %d, want %d", got.Offset, tt.wantOffset)
			}
		})
	}
}

func TestListFilter_Normalized_DoesNotMutateReceiver(t *testing.T) {
	orig := ListFilter{Query: "  raw  ", Limit: -1, Offset: -1}
	_ = orig.Normalized()
	if orig.Query != "  raw  " || orig.Limit != -1 || orig.Offset != -1 {
		t.Errorf("Normalized mutated receiver: %+v", orig)
	}
}
