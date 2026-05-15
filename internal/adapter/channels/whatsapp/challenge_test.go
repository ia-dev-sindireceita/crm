package whatsapp_test

import (
	"io"
	"net/http"
	"net/url"
	"testing"
)

func (k *testKit) get(t *testing.T, q url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, k.server.URL+"/webhooks/whatsapp?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := k.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestChallenge_OK(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	q := url.Values{}
	q.Set("hub.mode", "subscribe")
	q.Set("hub.verify_token", testVerifyToken)
	q.Set("hub.challenge", "echo-me-back-123")
	resp := k.get(t, q)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "echo-me-back-123" {
		t.Fatalf("body = %q", body)
	}
}

func TestChallenge_BadMode(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	q := url.Values{}
	q.Set("hub.mode", "unsubscribe")
	q.Set("hub.verify_token", testVerifyToken)
	q.Set("hub.challenge", "x")
	resp := k.get(t, q)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestChallenge_WrongToken(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	q := url.Values{}
	q.Set("hub.mode", "subscribe")
	q.Set("hub.verify_token", "WRONG")
	q.Set("hub.challenge", "x")
	resp := k.get(t, q)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestChallenge_EmptyToken(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	q := url.Values{}
	q.Set("hub.mode", "subscribe")
	q.Set("hub.challenge", "x")
	resp := k.get(t, q)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestChallenge_EchoIsEmpty(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	q := url.Values{}
	q.Set("hub.mode", "subscribe")
	q.Set("hub.verify_token", testVerifyToken)
	// hub.challenge omitted entirely.
	resp := k.get(t, q)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Fatalf("expected empty body, got %q", body)
	}
}
