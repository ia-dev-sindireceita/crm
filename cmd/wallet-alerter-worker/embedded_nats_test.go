package main

import (
	"net"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// runEmbeddedNATSForShim boots a JetStream-enabled nats-server bound to
// a free loopback port and returns its client URL. Same shape as the
// helper in internal/worker/wallet_alerter/integration_test.go; copied
// here on purpose to keep the shim test self-contained (and to avoid
// pulling a test-only helper across packages).
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
