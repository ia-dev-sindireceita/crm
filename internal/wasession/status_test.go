package wasession

import "testing"

func TestStatusValid(t *testing.T) {
	t.Parallel()
	valid := []Status{StatusUnpaired, StatusPairing, StatusConnected, StatusDisconnected, StatusBanned}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("Status(%q).Valid() = false, want true", s)
		}
	}
	for _, s := range []Status{"", "bogus", "CONNECTED"} {
		if s.Valid() {
			t.Errorf("Status(%q).Valid() = true, want false", s)
		}
	}
}

func TestStatusTerminal(t *testing.T) {
	t.Parallel()
	if !StatusBanned.Terminal() {
		t.Error("banned must be terminal")
	}
	for _, s := range []Status{StatusUnpaired, StatusPairing, StatusConnected, StatusDisconnected} {
		if s.Terminal() {
			t.Errorf("Status(%q).Terminal() = true, want false", s)
		}
	}
}

func TestStatusLive(t *testing.T) {
	t.Parallel()
	if !StatusConnected.Live() {
		t.Error("connected must be live")
	}
	for _, s := range []Status{StatusUnpaired, StatusPairing, StatusDisconnected, StatusBanned} {
		if s.Live() {
			t.Errorf("Status(%q).Live() = true, want false", s)
		}
	}
}

func TestStatusString(t *testing.T) {
	t.Parallel()
	if got := StatusConnected.String(); got != "connected" {
		t.Errorf("String() = %q, want connected", got)
	}
}
