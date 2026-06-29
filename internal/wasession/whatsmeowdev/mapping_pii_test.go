package whatsmeowdev

import (
	"strings"
	"testing"
)

// TestE164ToJIDErrorRedactsPII is the F3 regression test (SIN-66268): the
// not-numeric error path must not echo the raw recipient number (PII / LGPD)
// into the error string, since it may be logged downstream. The error must
// still be returned, but must not contain any digit of the input.
func TestE164ToJIDErrorRedactsPII(t *testing.T) {
	t.Parallel()
	const dirty = "5511A9876" // non-numeric, carries real-looking digits
	_, err := e164ToJID(dirty)
	if err == nil {
		t.Fatal("expected error for non-numeric recipient")
	}
	if strings.Contains(err.Error(), dirty) {
		t.Errorf("error %q echoes the raw recipient (PII leak)", err)
	}
	// Defence in depth: no contiguous run of the input digits should survive.
	for _, frag := range []string{"5511", "9876"} {
		if strings.Contains(err.Error(), frag) {
			t.Errorf("error %q leaks digit fragment %q", err, frag)
		}
	}
}

func TestRedactPhone(t *testing.T) {
	t.Parallel()
	got := redactPhone("5511A9876")
	if strings.Contains(got, "5511") {
		t.Errorf("redactPhone(%q) leaked digits: %q", "5511A9876", got)
	}
	if want := "[redacted len=9]"; got != want {
		t.Errorf("redactPhone = %q, want %q", got, want)
	}
}
