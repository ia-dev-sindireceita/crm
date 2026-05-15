// Package clamav implements scanner.MediaScanner against a clamd
// instance over TCP, using the INSTREAM protocol described in
// `man clamd` (sections "INSTREAM" and "Commands").
//
// The package is intentionally narrow:
//
//   - One Scanner per (Addr, Timeout, MaxChunkSize) tuple. Concrete
//     clamd connections are created on demand inside Scan — clamd
//     closes the TCP connection after each command, so connection
//     pooling buys nothing.
//   - Storage access is hexagonal: the adapter does not import any
//     S3/MinIO SDK. The caller injects a BlobReader port that hands
//     back an io.ReadCloser for a given storage key. That keeps
//     `internal/media/scanner` clean of vendor SDKs (per the F2-05a
//     port doc) and lets the worker compose this adapter with
//     whichever blob source production wires up.
//   - Each Scan does two dials: zVERSION (so the EngineID we persist
//     reflects the engine + signature DB version that produced *this*
//     verdict — important after `freshclam` reloads) and zINSTREAM
//     for the verdict itself. clamd allows only one command per TCP
//     connection, so this is the cheapest correct shape.
//
// Wire format reminder (big-endian):
//
//	zINSTREAM\0
//	<uint32 size><chunk bytes>   (repeated, size > 0)
//	<uint32 size=0>              (terminator)
//	<response bytes>\0           ("stream: OK", "stream: X FOUND", or "… ERROR")
package clamav

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/media/scanner"
)

// DefaultChunkSize is the INSTREAM chunk size used when Config.MaxChunkSize
// is zero. clamd's StreamMaxLength defaults to 25 MiB; 64 KiB keeps each
// chunk well below that and matches the size most clamav client libraries
// use.
const DefaultChunkSize = 64 * 1024

// DefaultTimeout caps both the dial and any subsequent read/write on the
// clamd connection when Config.Timeout is zero. Picked to be longer than
// a typical signature lookup but short enough that a stuck clamd does not
// stall the scan worker indefinitely.
const DefaultTimeout = 30 * time.Second

// DialFunc abstracts net.Dialer.DialContext so tests can swap a net.Pipe
// in for a real TCP socket without spinning up a listener.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Config configures a Scanner. Addr is the only required field; the
// remaining fields have sane defaults.
type Config struct {
	// Addr is the host:port clamd listens on (e.g. "clamav:3310").
	Addr string

	// Timeout caps both dial and per-connection I/O. Zero → DefaultTimeout.
	Timeout time.Duration

	// MaxChunkSize is the INSTREAM chunk size. Zero → DefaultChunkSize.
	MaxChunkSize int

	// Dial is optional; tests inject a net.Pipe-backed dialer here.
	// Production callers should leave it nil so the default
	// net.Dialer.DialContext is used.
	Dial DialFunc
}

// BlobReader is the storage port the adapter uses to fetch the object
// identified by a storage key (the same path produced by
// `internal/media/upload.StoragePath`). The port stays in this package
// rather than `internal/media/scanner` so the scanner port itself does
// not depend on `io` at all and so two competing adapters could read
// blobs from different stores without forcing the worker to know.
//
// Implementations MUST return a ReadCloser that is safe to close once,
// and MUST honour ctx for cancellation.
type BlobReader interface {
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// Scanner implements scanner.MediaScanner against a single clamd
// endpoint. Safe for concurrent use — each Scan call opens its own
// connections via cfg.Dial.
type Scanner struct {
	cfg   Config
	blobs BlobReader
}

// Compile-time guarantee the adapter satisfies the domain port. If the
// port drifts, this breaks the build before any test runs.
var _ scanner.MediaScanner = (*Scanner)(nil)

// New constructs a Scanner. Returns an error when Addr is empty or
// blobs is nil; defaults are applied to the other fields.
func New(cfg Config, blobs BlobReader) (*Scanner, error) {
	if cfg.Addr == "" {
		return nil, errors.New("clamav: Config.Addr is required")
	}
	if blobs == nil {
		return nil, errors.New("clamav: BlobReader is required")
	}
	if cfg.MaxChunkSize <= 0 {
		cfg.MaxChunkSize = DefaultChunkSize
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Dial == nil {
		d := &net.Dialer{}
		cfg.Dial = d.DialContext
	}
	return &Scanner{cfg: cfg, blobs: blobs}, nil
}

// Scan implements scanner.MediaScanner. The two-dial flow (VERSION then
// INSTREAM) is deliberate — see the package doc.
func (s *Scanner) Scan(ctx context.Context, key string) (scanner.ScanResult, error) {
	blob, err := s.blobs.Open(ctx, key)
	if err != nil {
		return scanner.ScanResult{}, fmt.Errorf("clamav: open blob %q: %w", key, err)
	}
	defer blob.Close()

	engineID, err := s.fetchVersion(ctx)
	if err != nil {
		return scanner.ScanResult{}, err
	}

	status, signature, err := s.streamScan(ctx, blob)
	if err != nil {
		return scanner.ScanResult{}, err
	}
	return scanner.ScanResult{Status: status, EngineID: engineID, Signature: signature}, nil
}

// fetchVersion runs zVERSION on a fresh connection and returns the
// canonical EngineID string ("clamav-<rest>") persisted by the worker.
func (s *Scanner) fetchVersion(ctx context.Context) (string, error) {
	conn, err := s.dial(ctx)
	if err != nil {
		return "", fmt.Errorf("clamav: dial (VERSION): %w", err)
	}
	defer conn.Close()
	s.applyDeadline(conn)

	if _, err := conn.Write([]byte("zVERSION\x00")); err != nil {
		return "", fmt.Errorf("clamav: write VERSION: %w", err)
	}
	resp, err := readUntilNull(conn)
	if err != nil {
		return "", fmt.Errorf("clamav: read VERSION: %w", err)
	}
	return parseVersion(resp), nil
}

// streamScan runs the INSTREAM exchange and returns the parsed verdict
// plus the threat signature (empty on clean verdicts; engine-defined
// string like "Eicar-Signature" on FOUND).
func (s *Scanner) streamScan(ctx context.Context, blob io.Reader) (scanner.Status, string, error) {
	conn, err := s.dial(ctx)
	if err != nil {
		return "", "", fmt.Errorf("clamav: dial (INSTREAM): %w", err)
	}
	defer conn.Close()
	s.applyDeadline(conn)

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return "", "", fmt.Errorf("clamav: write INSTREAM cmd: %w", err)
	}

	if err := streamChunks(ctx, conn, blob, s.cfg.MaxChunkSize); err != nil {
		return "", "", err
	}

	resp, err := readUntilNull(conn)
	if err != nil {
		return "", "", fmt.Errorf("clamav: read verdict: %w", err)
	}
	return parseVerdict(resp)
}

// streamChunks pumps blob -> conn as <uint32 BE size><chunk> pairs and
// terminates with a zero-sized chunk. Returns an error from any
// underlying I/O or ctx cancellation.
func streamChunks(ctx context.Context, conn net.Conn, blob io.Reader, maxChunk int) error {
	buf := make([]byte, maxChunk)
	header := make([]byte, 4)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := blob.Read(buf)
		if n > 0 {
			binary.BigEndian.PutUint32(header, uint32(n))
			if _, err := conn.Write(header); err != nil {
				return fmt.Errorf("clamav: write chunk size: %w", err)
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return fmt.Errorf("clamav: write chunk body: %w", err)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return fmt.Errorf("clamav: read blob: %w", readErr)
		}
	}
	binary.BigEndian.PutUint32(header, 0)
	if _, err := conn.Write(header); err != nil {
		return fmt.Errorf("clamav: write terminator: %w", err)
	}
	return nil
}

func (s *Scanner) dial(ctx context.Context) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	return s.cfg.Dial(dialCtx, "tcp", s.cfg.Addr)
}

func (s *Scanner) applyDeadline(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(s.cfg.Timeout))
}

// readUntilNull consumes bytes from r up to (and including) the next
// NUL byte, returning the bytes without the NUL. A premature EOF —
// either with no bytes at all or with bytes but no NUL — is an error;
// clamd always terminates its responses with NUL when the request used
// the `z` prefix.
func readUntilNull(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	s, err := br.ReadString(0)
	if err != nil {
		if s == "" {
			return "", err
		}
		return "", fmt.Errorf("premature EOF before NUL terminator: %w", err)
	}
	return strings.TrimRight(s, "\x00"), nil
}

// parseVersion maps clamd's VERSION line to the EngineID we persist.
// clamd typically replies "ClamAV 1.3.0/27450/Thu Jun 13 09:36:39 2024";
// we normalise to "clamav-1.3.0/27450/Thu Jun 13 09:36:39 2024" so the
// stored EngineID has a stable lowercase prefix and still carries the
// signature DB version for audit.
func parseVersion(s string) string {
	s = strings.TrimSpace(s)
	const prefix = "ClamAV "
	if strings.HasPrefix(s, prefix) {
		return "clamav-" + s[len(prefix):]
	}
	return "clamav-" + s
}

// parseVerdict maps clamd's INSTREAM reply to a domain Status plus the
// per-threat signature (empty on clean verdicts). ERROR responses are
// surfaced as a Go error so the worker can retry per its own policy
// rather than persisting StatusClean on a transport failure.
//
// clamd's FOUND line has the shape "stream: <signature> FOUND" — we
// strip the "stream:" prefix and the trailing " FOUND" so the caller
// gets a clean signature string suitable for the alert.Event payload.
func parseVerdict(s string) (scanner.Status, string, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.Contains(s, "ERROR"):
		return "", "", fmt.Errorf("clamav: engine error: %s", s)
	case strings.HasSuffix(s, " OK"):
		return scanner.StatusClean, "", nil
	case strings.HasSuffix(s, " FOUND"):
		return scanner.StatusInfected, extractSignature(s), nil
	default:
		return "", "", fmt.Errorf("clamav: unexpected response %q", s)
	}
}

// extractSignature strips the "stream: " prefix and " FOUND" suffix
// from a clamd INSTREAM verdict line, returning the threat name. Falls
// back to the empty string when the line shape is unexpected; the
// alerter then renders an "unknown signature" placeholder.
func extractSignature(line string) string {
	trimmed := strings.TrimSuffix(line, " FOUND")
	trimmed = strings.TrimPrefix(trimmed, "stream: ")
	trimmed = strings.TrimSpace(trimmed)
	return trimmed
}
