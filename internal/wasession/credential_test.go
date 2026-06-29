package wasession

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const secret = "2@superSecretPairingCode==,key/with+chars"

func TestCredentialRevealAndZero(t *testing.T) {
	t.Parallel()
	c := NewCredential(secret)
	if c.Reveal() != secret {
		t.Fatalf("Reveal() = %q, want the secret", c.Reveal())
	}
	if c.IsZero() {
		t.Error("non-empty credential reports IsZero")
	}
	if !NewCredential("").IsZero() {
		t.Error("empty credential should be zero")
	}
}

func TestCredentialRedactsEveryRendering(t *testing.T) {
	t.Parallel()
	c := NewCredential(secret)
	renders := map[string]string{
		"String":   c.String(),
		"GoString": c.GoString(),
		"%v":       fmt.Sprintf("%v", c),
		"%s":       fmt.Sprintf("%s", c),
		"%q":       fmt.Sprintf("%q", c),
		"%#v":      fmt.Sprintf("%#v", c),
		"%+v":      fmt.Sprintf("%+v", c),
		"%d":       fmt.Sprintf("%d", c),
	}
	for name, out := range renders {
		if strings.Contains(out, "superSecret") || strings.Contains(out, "key/with") {
			t.Errorf("%s leaked the secret: %q", name, out)
		}
		if !strings.Contains(out, redacted) {
			t.Errorf("%s = %q, want it to contain %q", name, out, redacted)
		}
	}
}

func TestCredentialMarshalJSONRedacts(t *testing.T) {
	t.Parallel()
	type wrap struct {
		QR Credential `json:"qr"`
	}
	b, err := json.Marshal(wrap{QR: NewCredential(secret)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("superSecret")) {
		t.Fatalf("JSON leaked secret: %s", b)
	}
	if !bytes.Contains(b, []byte(redacted)) {
		t.Fatalf("JSON = %s, want redacted", b)
	}
}

func TestCredentialSlogRedacts(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	l.Info("pairing", "qr", NewCredential(secret))
	if strings.Contains(buf.String(), "superSecret") {
		t.Fatalf("slog leaked secret: %s", buf.String())
	}
	if !strings.Contains(buf.String(), redacted) {
		t.Fatalf("slog = %s, want redacted", buf.String())
	}
}
