package csrf

import (
	"strings"
	"testing"
)

func TestFormHidden(t *testing.T) {
	got := string(FormHidden("tok-1"))
	want := `<input type="hidden" name="_csrf" value="tok-1">`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestFormHidden_EscapesAttackerToken(t *testing.T) {
	// A token forged with quote and angle-bracket characters MUST NOT
	// be able to escape the attribute value.
	got := string(FormHidden(`x"><script>alert(1)</script>`))
	if strings.Contains(got, "<script>") {
		t.Fatalf("FormHidden failed to escape: %q", got)
	}
	if !strings.Contains(got, "&#34;") && !strings.Contains(got, "&quot;") {
		t.Fatalf("FormHidden did not escape the quote: %q", got)
	}
}

func TestMetaTag(t *testing.T) {
	got := string(MetaTag("tok-2"))
	want := `<meta name="csrf-token" content="tok-2">`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestMetaTag_EscapesAttackerToken(t *testing.T) {
	got := string(MetaTag(`"><img src=x onerror=alert(1)>`))
	if strings.Contains(got, "<img") {
		t.Fatalf("MetaTag failed to escape: %q", got)
	}
}

func TestHXHeadersAttr(t *testing.T) {
	got := string(HXHeadersAttr("tok-3"))
	want := `hx-headers='{"X-CSRF-Token": "tok-3"}'`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHXHeadersAttr_EscapesAttackerToken(t *testing.T) {
	// A token containing " or </script> must not be able to break out
	// of the JSON-quoted value or break the surrounding hx-headers
	// single-quoted attribute.
	got := string(HXHeadersAttr(`"</script><script>alert(1)</script>`))
	if strings.Contains(got, "</script>") {
		t.Fatalf("HXHeadersAttr failed to escape </script>: %q", got)
	}
}
