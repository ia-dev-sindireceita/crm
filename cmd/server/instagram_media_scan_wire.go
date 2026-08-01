package main

// Instagram media-scan publisher — closes the "MediaScanPublisher wired
// in a follow-up PR" gap left by instagram_wire.go. Without this, every
// inbound Instagram attachment's media.scan.requested envelope was never
// published, on top of the uuid.Nil MessageID bug fixed in the same PR
// this wire lands with.
//
// Mirrors funnel_engine_publisher_wire.go's shape but dials its own,
// independent NATS connection rather than sharing the funnel-engine
// publisher's — simpler and zero risk to that already-working path.
// NATS connections are cheap; a second one per process is not a
// meaningful cost.

import (
	"context"
	"errors"
	"log"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
)

const envInstagramMediaScanPublisherName = "crm-instagram-media-scan-publisher"

// instagramMediaScanPublisherConnect is the test seam for the NATS dial.
// Production binds it to natsadapter.Connect; unit tests inject a fake.
type instagramMediaScanPublisherConnect func(ctx context.Context, cfg natsadapter.SDKConfig) (*natsadapter.SDKAdapter, error)

// buildInstagramMediaScanPublisher dials NATS using the shared env vars
// (NATS_URL + auth/TLS family) and wraps the SDKAdapter in a
// MediaScanRequestPublisher. Returns (nil, nil, nil) when NATS_URL is
// unset — the caller treats that as "disabled, skip wiring".
func buildInstagramMediaScanPublisher(ctx context.Context, getenv func(string) string) (*natsadapter.MediaScanRequestPublisher, func(), error) {
	return buildInstagramMediaScanPublisherWithConnect(ctx, getenv, defaultInstagramMediaScanPublisherConnect)
}

func buildInstagramMediaScanPublisherWithConnect(
	ctx context.Context,
	getenv func(string) string,
	connect instagramMediaScanPublisherConnect,
) (*natsadapter.MediaScanRequestPublisher, func(), error) {
	natsURL := getenv(envNATSURL)
	if natsURL == "" {
		return nil, nil, nil
	}
	cfg := natsadapter.SDKConfig{
		URL:         natsURL,
		Name:        envInstagramMediaScanPublisherName,
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
	pub, err := natsadapter.NewMediaScanRequestPublisher(a)
	if err != nil {
		a.Close()
		return nil, nil, err
	}
	cleanup := func() {
		if drainErr := a.Drain(); drainErr != nil {
			log.Printf("crm: instagram media scan publisher drain: %v", drainErr)
		}
	}
	return pub, cleanup, nil
}

func defaultInstagramMediaScanPublisherConnect(ctx context.Context, cfg natsadapter.SDKConfig) (*natsadapter.SDKAdapter, error) {
	a, err := natsadapter.Connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, errors.New("nats: connect returned nil adapter")
	}
	return a, nil
}
