package main

// Test-only helper to bring up an embedded JetStream NATS server.
// Mirrors the same helper in cmd/wallet-alerter-worker so the shim
// smoke test in main_test.go does not depend on internal/worker/...
// test artifacts.

import (
	"net"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

func runEmbeddedNATSForShim(t *testing.T) string {
	t.Helper()
	port := pickFreePortForShim(t)
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      port,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		s.Shutdown()
		s.WaitForShutdown()
	})
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats-server not ready in time")
	}
	return s.ClientURL()
}

func pickFreePortForShim(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
