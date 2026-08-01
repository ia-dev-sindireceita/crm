package main

// Wire-up tests for buildInstagramMediaScanPublisher. Cover the env
// gating + dial-error soft-fail branch without dialing a real NATS
// server, mirroring funnel_engine_publisher_wire_test.go.

import (
	"context"
	"errors"
	"testing"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
)

func TestBuildInstagramMediaScanPublisher_NATSURLUnset_ReturnsNilTriple(t *testing.T) {
	pub, cleanup, err := buildInstagramMediaScanPublisherWithConnect(
		context.Background(),
		funnelEnv(map[string]string{}),
		func(_ context.Context, _ natsadapter.SDKConfig) (*natsadapter.SDKAdapter, error) {
			t.Fatalf("connect must not be called when NATS_URL is unset")
			return nil, nil
		},
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if pub != nil {
		t.Fatalf("pub = %v, want nil (disabled)", pub)
	}
	if cleanup != nil {
		t.Fatalf("cleanup non-nil, want nil (disabled)")
	}
}

func TestBuildInstagramMediaScanPublisher_ConnectError_PropagatesAndReturnsNilPublisher(t *testing.T) {
	sentinel := errors.New("nats: dial refused")
	pub, cleanup, err := buildInstagramMediaScanPublisherWithConnect(
		context.Background(),
		funnelEnv(map[string]string{envNATSURL: "tls://nats:4222"}),
		func(_ context.Context, _ natsadapter.SDKConfig) (*natsadapter.SDKAdapter, error) {
			return nil, sentinel
		},
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if pub != nil {
		t.Fatalf("pub = %v, want nil on connect error", pub)
	}
	if cleanup != nil {
		t.Fatalf("cleanup non-nil, want nil on connect error")
	}
}

func TestBuildInstagramMediaScanPublisher_HonoursAuthEnvAndDedicatedName(t *testing.T) {
	captured := natsadapter.SDKConfig{}
	connect := func(_ context.Context, cfg natsadapter.SDKConfig) (*natsadapter.SDKAdapter, error) {
		captured = cfg
		return nil, errors.New("stop here so we don't dial")
	}
	env := map[string]string{
		envNATSURL:      "tls://nats.example:4222",
		envNATSToken:    "tok",
		envNATSCreds:    "/run/creds.creds",
		envNATSTLSCA:    "/run/ca.pem",
		envNATSTLSCert:  "/run/cert.pem",
		envNATSTLSKey:   "/run/key.pem",
		envNATSInsecure: "1",
	}
	_, _, err := buildInstagramMediaScanPublisherWithConnect(context.Background(), funnelEnv(env), connect)
	if err == nil {
		t.Fatalf("want stub error, got nil")
	}
	if captured.URL != env[envNATSURL] {
		t.Errorf("URL = %q, want %q", captured.URL, env[envNATSURL])
	}
	if captured.Token != env[envNATSToken] {
		t.Errorf("Token = %q, want %q", captured.Token, env[envNATSToken])
	}
	if !captured.Insecure {
		t.Errorf("Insecure = false, want true (NATS_INSECURE=1)")
	}
	// Distinct connection name from the funnel-engine publisher — this
	// is a second, independent NATS connection, not a shared one.
	if captured.Name != envInstagramMediaScanPublisherName {
		t.Errorf("Name = %q, want %q", captured.Name, envInstagramMediaScanPublisherName)
	}
	if captured.Name == envFunnelEnginePublisherName {
		t.Errorf("Name must not equal the funnel-engine publisher's name")
	}
}
