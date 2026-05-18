package main

// SIN-62960 — wire-up tests for buildFunnelEngineInboundPublisher.
// Cover the env gating + dial-error soft-fail branch without dialing a
// real NATS server. The connector is injected via the *WithConnect
// overload that production binds to natsadapter.Connect.

import (
	"context"
	"errors"
	"testing"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
)

func funnelEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestBuildFunnelEngineInboundPublisher_NATSURLUnset_ReturnsNilTriple(t *testing.T) {
	pub, cleanup, err := buildFunnelEngineInboundPublisherWithConnect(
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

func TestBuildFunnelEngineInboundPublisher_ConnectError_PropagatesAndReturnsNilPublisher(t *testing.T) {
	sentinel := errors.New("nats: dial refused")
	pub, cleanup, err := buildFunnelEngineInboundPublisherWithConnect(
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

func TestBuildFunnelEngineInboundPublisher_HonoursAuthEnv(t *testing.T) {
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
	_, _, err := buildFunnelEngineInboundPublisherWithConnect(context.Background(), funnelEnv(env), connect)
	if err == nil {
		t.Fatalf("want stub error, got nil")
	}
	if captured.URL != env[envNATSURL] {
		t.Errorf("URL = %q, want %q", captured.URL, env[envNATSURL])
	}
	if captured.Token != env[envNATSToken] {
		t.Errorf("Token = %q, want %q", captured.Token, env[envNATSToken])
	}
	if captured.CredsFile != env[envNATSCreds] {
		t.Errorf("CredsFile = %q, want %q", captured.CredsFile, env[envNATSCreds])
	}
	if captured.TLSCAFile != env[envNATSTLSCA] {
		t.Errorf("TLSCAFile = %q, want %q", captured.TLSCAFile, env[envNATSTLSCA])
	}
	if captured.TLSCertFile != env[envNATSTLSCert] {
		t.Errorf("TLSCertFile = %q, want %q", captured.TLSCertFile, env[envNATSTLSCert])
	}
	if captured.TLSKeyFile != env[envNATSTLSKey] {
		t.Errorf("TLSKeyFile = %q, want %q", captured.TLSKeyFile, env[envNATSTLSKey])
	}
	if !captured.Insecure {
		t.Errorf("Insecure = false, want true (NATS_INSECURE=1)")
	}
	if captured.Name != envFunnelEnginePublisherName {
		t.Errorf("Name = %q, want %q", captured.Name, envFunnelEnginePublisherName)
	}
}
