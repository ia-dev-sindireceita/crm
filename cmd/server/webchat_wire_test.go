package main

import "testing"

// buildWebchatHandler dials Postgres through the shared IAM pool; the
// happy-path assembly (ReceiveInbound → Postgres) is covered end-to-end
// by the integration tests in internal/adapter/channels/webchat and the
// postgres adapter tests. Here we pin the fail-soft contract: a nil pool
// yields (nil, nil) so partial-stack boots simply omit /widget/v1/*.
func TestBuildWebchatHandler_NilPool_ReturnsNil(t *testing.T) {
	t.Parallel()
	h, err := buildWebchatHandler(nil, func(string) string { return "" })
	if err != nil {
		t.Fatalf("buildWebchatHandler(nil) err = %v, want nil", err)
	}
	if h != nil {
		t.Fatalf("buildWebchatHandler(nil) = %v, want nil handler", h)
	}
}
