package main

// SIN-62960 — funnel engine inbound publisher.
//
// The inbox use case (ReceiveInbound) optionally fans persisted inbound
// messages onto the JetStream subject [engine.Subject] so the funnel
// rule engine (cmd/funnel-engine-worker, a separate process) can
// evaluate rules and apply actions. This file builds the publisher
// adapter from env config and exposes a single helper that channel
// wires (whatsapp_wire.go, messenger_wire.go) call after constructing
// their ReceiveInbound use case.
//
// Lifecycle / contract:
//   - Returns nil publisher when NATS_URL is unset. The caller treats
//     nil as "skip wiring"; ReceiveInbound's publish hook degrades to a
//     no-op (the inbox keeps working — bus is a notification, not the
//     source of truth).
//   - On success returns (publisher, cleanup). The cleanup function
//     drains the underlying SDKAdapter and MUST be invoked at process
//     shutdown to ack any in-flight publishes.
//   - EnsureStream is owned by the consumer (cmd/funnel-engine-worker
//     runs it on startup so the stream definition lives with the worker
//     that pins its retention / replicas). The publisher only writes —
//     it does not configure topology.

import (
	"context"
	"errors"
	"log"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
)

const envFunnelEnginePublisherName = "crm-funnel-engine-publisher"

// funnelEnginePublisherConnect is the test seam for the NATS dial.
// Production binds it to natsadapter.Connect; unit tests inject a fake.
type funnelEnginePublisherConnect func(ctx context.Context, cfg natsadapter.SDKConfig) (*natsadapter.SDKAdapter, error)

// buildFunnelEngineInboundPublisher dials NATS using the shared env vars
// (NATS_URL + auth/TLS family) and wraps the SDKAdapter in an
// InboundMessagePublisher. Returns (nil, nil, nil) when NATS_URL is
// unset — the caller treats that as "disabled, skip wiring". Any
// validation / dial error is returned so the caller logs and continues
// (channel wires already soft-fail on optional dependencies).
func buildFunnelEngineInboundPublisher(ctx context.Context, getenv func(string) string) (*natsadapter.InboundMessagePublisher, func(), error) {
	return buildFunnelEngineInboundPublisherWithConnect(ctx, getenv, defaultFunnelEnginePublisherConnect)
}

func buildFunnelEngineInboundPublisherWithConnect(
	ctx context.Context,
	getenv func(string) string,
	connect funnelEnginePublisherConnect,
) (*natsadapter.InboundMessagePublisher, func(), error) {
	natsURL := getenv(envNATSURL)
	if natsURL == "" {
		return nil, nil, nil
	}
	cfg := natsadapter.SDKConfig{
		URL:         natsURL,
		Name:        envFunnelEnginePublisherName,
		Token:       getenv(envNATSToken),
		NKeyFile:    getenv(envNATSNKey),
		CredsFile:   getenv(envNATSCreds),
		TLSCAFile:   getenv(envNATSTLSCA),
		TLSCertFile: getenv(envNATSTLSCert),
		TLSKeyFile:  getenv(envNATSTLSKey),
		Insecure:    truthyEnv(getenv(envNATSInsecure)),
	}
	a, err := connect(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	pub, err := natsadapter.NewInboundMessagePublisher(a)
	if err != nil {
		a.Close()
		return nil, nil, err
	}
	cleanup := func() {
		if drainErr := a.Drain(); drainErr != nil {
			log.Printf("crm: funnel engine publisher drain: %v", drainErr)
		}
	}
	return pub, cleanup, nil
}

func defaultFunnelEnginePublisherConnect(ctx context.Context, cfg natsadapter.SDKConfig) (*natsadapter.SDKAdapter, error) {
	a, err := natsadapter.Connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, errors.New("nats: connect returned nil adapter")
	}
	return a, nil
}
