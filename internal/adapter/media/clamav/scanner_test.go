package clamav_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/media/clamav"
	"github.com/pericles-luz/crm/internal/media/scanner"
)

// EICAR is the standard antivirus test string. It is matched by the
// ClamAV signature `Eicar-Signature` (engine version varies). We use
// the *first* 30 bytes (the canonical EICAR header) as the blob body
// in the infected case so the test reads naturally even though the
// stub never actually scans.
const eicarHead = `X5O!P%@AP[4\PZX54(P^)`

// --- BlobReader fake ---------------------------------------------------

type fakeBlobs struct {
	data map[string][]byte
	err  error
}

func (f *fakeBlobs) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.data[key]
	if !ok {
		return nil, fmt.Errorf("fakeBlobs: not found: %s", key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// --- clamd stub over net.Pipe -----------------------------------------

// handlerFn is one server-side conversation. clamd closes the
// connection after each command, so one handler == one TCP exchange.
type handlerFn func(t *testing.T, server net.Conn)

// chainDialer hands out fresh net.Pipe pairs and invokes handlers in
// order — one handler per dial. Scan dials twice (VERSION then
// INSTREAM), so a typical case supplies two handlers.
func chainDialer(t *testing.T, handlers ...handlerFn) clamav.DialFunc {
	t.Helper()
	ch := make(chan handlerFn, len(handlers))
	for _, h := range handlers {
		ch <- h
	}
	close(ch)
	return func(_ context.Context, _, _ string) (net.Conn, error) {
		h, ok := <-ch
		if !ok {
			return nil, errors.New("chainDialer: too many dials")
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			h(t, server)
		}()
		return client, nil
	}
}

// readNullCmd reads a single null-terminated command word from conn
// (e.g. "zVERSION" or "zINSTREAM").
func readNullCmd(conn net.Conn) (string, error) {
	br := bufio.NewReader(conn)
	s, err := br.ReadString(0)
	if err != nil {
		return "", err
	}
	// Strip leading 'z' and trailing NUL.
	return strings.TrimRight(s, "\x00"), nil
}

// readChunks reads <uint32 BE size><body> pairs from conn until size=0.
// Returns the concatenated body or an error. Tests use this to assert
// the adapter shipped the full blob exactly once and terminated with
// a zero-size marker.
func readChunks(conn net.Conn) ([]byte, error) {
	var out bytes.Buffer
	header := make([]byte, 4)
	for {
		if _, err := io.ReadFull(conn, header); err != nil {
			return nil, fmt.Errorf("chunk header: %w", err)
		}
		n := binary.BigEndian.Uint32(header)
		if n == 0 {
			return out.Bytes(), nil
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(conn, body); err != nil {
			return nil, fmt.Errorf("chunk body: %w", err)
		}
		out.Write(body)
	}
}

// handlerVersion replies to a single zVERSION exchange with versionLine.
func handlerVersion(versionLine string) handlerFn {
	return func(t *testing.T, server net.Conn) {
		t.Helper()
		cmd, err := readNullCmd(server)
		if err != nil {
			t.Errorf("VERSION: read cmd: %v", err)
			return
		}
		if cmd != "zVERSION" {
			t.Errorf("VERSION: cmd = %q, want %q", cmd, "zVERSION")
			return
		}
		if _, err := server.Write([]byte(versionLine + "\x00")); err != nil {
			t.Errorf("VERSION: write: %v", err)
		}
	}
}

// handlerINSTREAM reads a full INSTREAM exchange, optionally validates
// the received payload, and replies with `reply`.
func handlerINSTREAM(wantPayload []byte, reply string) handlerFn {
	return func(t *testing.T, server net.Conn) {
		t.Helper()
		cmd, err := readNullCmd(server)
		if err != nil {
			t.Errorf("INSTREAM: read cmd: %v", err)
			return
		}
		if cmd != "zINSTREAM" {
			t.Errorf("INSTREAM: cmd = %q, want %q", cmd, "zINSTREAM")
			return
		}
		got, err := readChunks(server)
		if err != nil {
			t.Errorf("INSTREAM: read chunks: %v", err)
			return
		}
		if wantPayload != nil && !bytes.Equal(got, wantPayload) {
			t.Errorf("INSTREAM: payload mismatch:\n got: %q\nwant: %q", got, wantPayload)
		}
		if _, err := server.Write([]byte(reply + "\x00")); err != nil {
			t.Errorf("INSTREAM: write reply: %v", err)
		}
	}
}

// handlerHang reads the command then blocks on a never-resolving read
// so the client-side SetDeadline trips a timeout.
func handlerHang() handlerFn {
	return func(t *testing.T, server net.Conn) {
		t.Helper()
		_, _ = readNullCmd(server)
		// Block until the pipe is closed by the client deadline.
		_, _ = io.ReadAll(server)
	}
}

// --- tests ------------------------------------------------------------

func TestScan_Clean(t *testing.T) {
	t.Parallel()
	const key = "media/t/2026-05/abc.png"
	payload := []byte("clean-bytes-here")
	blobs := &fakeBlobs{data: map[string][]byte{key: payload}}

	s, err := clamav.New(clamav.Config{
		Addr: "stub:3310",
		Dial: chainDialer(t,
			handlerVersion("ClamAV 1.4.2/27450/Thu Jun 13 09:36:39 2024"),
			handlerINSTREAM(payload, "stream: OK"),
		),
	}, blobs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := s.Scan(context.Background(), key)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := scanner.ScanResult{
		Status:   scanner.StatusClean,
		EngineID: "clamav-1.4.2/27450/Thu Jun 13 09:36:39 2024",
	}
	if got != want {
		t.Fatalf("Scan = %+v, want %+v", got, want)
	}
}

func TestScan_InfectedEICAR(t *testing.T) {
	t.Parallel()
	const key = "media/t/2026-05/eicar.txt"
	payload := []byte(eicarHead)
	blobs := &fakeBlobs{data: map[string][]byte{key: payload}}

	s, err := clamav.New(clamav.Config{
		Addr: "stub:3310",
		Dial: chainDialer(t,
			handlerVersion("ClamAV 1.4.2/27450/x"),
			handlerINSTREAM(payload, "stream: Eicar-Signature FOUND"),
		),
	}, blobs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := s.Scan(context.Background(), key)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got.Status != scanner.StatusInfected {
		t.Errorf("Status = %q, want %q", got.Status, scanner.StatusInfected)
	}
	if got.EngineID != "clamav-1.4.2/27450/x" {
		t.Errorf("EngineID = %q, want %q", got.EngineID, "clamav-1.4.2/27450/x")
	}
}

func TestScan_ChunkedStream(t *testing.T) {
	t.Parallel()
	const key = "media/t/2026-05/big.bin"
	// 3.5 chunks at MaxChunkSize=10 -> exercises the loop boundary.
	payload := bytes.Repeat([]byte("AB"), 17) // 34 bytes
	blobs := &fakeBlobs{data: map[string][]byte{key: payload}}

	s, err := clamav.New(clamav.Config{
		Addr:         "stub:3310",
		MaxChunkSize: 10,
		Dial: chainDialer(t,
			handlerVersion("ClamAV 1.3.0"),
			handlerINSTREAM(payload, "stream: OK"),
		),
	}, blobs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := s.Scan(context.Background(), key)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got.Status != scanner.StatusClean {
		t.Errorf("Status = %q, want clean", got.Status)
	}
	if got.EngineID != "clamav-1.3.0" {
		t.Errorf("EngineID = %q, want clamav-1.3.0", got.EngineID)
	}
}

func TestScan_EngineError(t *testing.T) {
	t.Parallel()
	const key = "media/t/2026-05/big.bin"
	payload := []byte("anything")
	blobs := &fakeBlobs{data: map[string][]byte{key: payload}}

	s, err := clamav.New(clamav.Config{
		Addr: "stub:3310",
		Dial: chainDialer(t,
			handlerVersion("ClamAV 1.4.2"),
			handlerINSTREAM(payload, "INSTREAM size limit exceeded. ERROR"),
		),
	}, blobs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := s.Scan(context.Background(), key)
	if err == nil {
		t.Fatalf("Scan err = nil, want engine error; got %+v", got)
	}
	if got != (scanner.ScanResult{}) {
		t.Errorf("ScanResult on error = %+v, want zero", got)
	}
	if !strings.Contains(err.Error(), "ERROR") {
		t.Errorf("err = %v, want contains ERROR", err)
	}
}

func TestScan_UnexpectedReply(t *testing.T) {
	t.Parallel()
	const key = "media/t/2026-05/x"
	blobs := &fakeBlobs{data: map[string][]byte{key: []byte("a")}}

	s, _ := clamav.New(clamav.Config{
		Addr: "stub:3310",
		Dial: chainDialer(t,
			handlerVersion("ClamAV 1.4.2"),
			handlerINSTREAM(nil, "stream: weird thing"),
		),
	}, blobs)
	_, err := s.Scan(context.Background(), key)
	if err == nil {
		t.Fatal("Scan err = nil, want unexpected-response error")
	}
	if !strings.Contains(err.Error(), "unexpected response") {
		t.Errorf("err = %v, want unexpected response", err)
	}
}

func TestScan_TimeoutOnVerdict(t *testing.T) {
	t.Parallel()
	const key = "media/t/2026-05/x"
	blobs := &fakeBlobs{data: map[string][]byte{key: []byte("a")}}

	// VERSION replies; INSTREAM hangs after the cmd read so the
	// client's deadline trips during streamChunks / verdict read.
	s, _ := clamav.New(clamav.Config{
		Addr:    "stub:3310",
		Timeout: 25 * time.Millisecond,
		Dial: chainDialer(t,
			handlerVersion("ClamAV 1.4.2"),
			handlerHang(),
		),
	}, blobs)
	start := time.Now()
	_, err := s.Scan(context.Background(), key)
	if err == nil {
		t.Fatal("Scan err = nil, want timeout")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Scan took %v, expected <1s thanks to deadline", d)
	}
}

func TestScan_VersionPrematureClose(t *testing.T) {
	t.Parallel()
	const key = "media/t/2026-05/x"
	blobs := &fakeBlobs{data: map[string][]byte{key: []byte("a")}}

	// VERSION handler reads cmd then closes with no payload at all.
	s, _ := clamav.New(clamav.Config{
		Addr: "stub:3310",
		Dial: chainDialer(t,
			func(_ *testing.T, server net.Conn) {
				_, _ = readNullCmd(server)
				// no write, just close on return
			},
		),
	}, blobs)
	_, err := s.Scan(context.Background(), key)
	if err == nil {
		t.Fatal("Scan err = nil, want VERSION error on premature close")
	}
	if !strings.Contains(err.Error(), "VERSION") {
		t.Errorf("err = %v, want VERSION-stage error", err)
	}
}

func TestScan_VersionTrailingBytesWithoutNull(t *testing.T) {
	t.Parallel()
	const key = "media/t/2026-05/x"
	blobs := &fakeBlobs{data: map[string][]byte{key: []byte("a")}}

	// Server writes bytes but never terminates with NUL.
	s, _ := clamav.New(clamav.Config{
		Addr: "stub:3310",
		Dial: chainDialer(t,
			func(_ *testing.T, server net.Conn) {
				_, _ = readNullCmd(server)
				_, _ = server.Write([]byte("ClamAV 1.4.2 partial"))
				// close without NUL terminator
			},
		),
	}, blobs)
	_, err := s.Scan(context.Background(), key)
	if err == nil {
		t.Fatal("Scan err = nil, want premature-EOF error")
	}
	if !strings.Contains(err.Error(), "premature EOF") {
		t.Errorf("err = %v, want premature EOF", err)
	}
}

func TestScan_BlobOpenError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("blob unavailable")
	blobs := &fakeBlobs{err: wantErr}
	s, _ := clamav.New(clamav.Config{
		Addr: "stub:3310",
		Dial: chainDialer(t), // never dials
	}, blobs)
	_, err := s.Scan(context.Background(), "any-key")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Scan err = %v, want wraps %v", err, wantErr)
	}
}

type errReader struct{ err error }

func (e *errReader) Read(_ []byte) (int, error) { return 0, e.err }
func (e *errReader) Close() error               { return nil }

type errBlobs struct{ err error }

func (e *errBlobs) Open(_ context.Context, _ string) (io.ReadCloser, error) {
	return &errReader{err: e.err}, nil
}

func TestScan_BlobReadError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("disk gremlins")
	s, _ := clamav.New(clamav.Config{
		Addr: "stub:3310",
		Dial: chainDialer(t,
			handlerVersion("ClamAV 1.4.2"),
			func(_ *testing.T, server net.Conn) {
				// Read the INSTREAM cmd, then the client should bail
				// during streaming and close the pipe.
				_, _ = readNullCmd(server)
				_, _ = io.ReadAll(server)
			},
		),
	}, &errBlobs{err: wantErr})
	_, err := s.Scan(context.Background(), "any-key")
	if err == nil {
		t.Fatal("Scan err = nil, want blob read error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wraps %v", err, wantErr)
	}
}

func TestScan_ContextCancelledDuringStream(t *testing.T) {
	t.Parallel()
	const key = "media/t/2026-05/big.bin"
	payload := bytes.Repeat([]byte("X"), 1024)
	blobs := &fakeBlobs{data: map[string][]byte{key: payload}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Scan

	s, _ := clamav.New(clamav.Config{
		Addr: "stub:3310",
		Dial: chainDialer(t,
			handlerVersion("ClamAV 1.4.2"),
			func(_ *testing.T, server net.Conn) {
				_, _ = readNullCmd(server)
				_, _ = io.ReadAll(server)
			},
		),
	}, blobs)
	_, err := s.Scan(ctx, key)
	if err == nil {
		t.Fatal("Scan err = nil, want context cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		cfg   clamav.Config
		blobs clamav.BlobReader
	}{
		{name: "empty addr", cfg: clamav.Config{}, blobs: &fakeBlobs{}},
		{name: "nil blobs", cfg: clamav.Config{Addr: "x:1"}, blobs: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := clamav.New(tc.cfg, tc.blobs)
			if err == nil {
				t.Fatalf("New = %+v, want error", s)
			}
		})
	}
}

func TestNew_Defaults(t *testing.T) {
	t.Parallel()
	// With Dial nil we rely on the constructor wiring a real net.Dialer
	// but we never actually dial — just confirm construction succeeds
	// and Scan with a hard-fail dialer surfaces an error rather than
	// panicking.
	s, err := clamav.New(clamav.Config{Addr: "127.0.0.1:1"}, &fakeBlobs{data: map[string][]byte{"k": []byte("d")}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// We cannot actually call Scan here without binding a port; the
	// goal is only to verify defaults are applied without panic. A
	// successful construction is enough.
	_ = s
}

func TestScan_DialError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("network unreachable")
	blobs := &fakeBlobs{data: map[string][]byte{"k": []byte("d")}}
	s, _ := clamav.New(clamav.Config{
		Addr: "stub:3310",
		Dial: func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, wantErr
		},
	}, blobs)
	_, err := s.Scan(context.Background(), "k")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Scan err = %v, want wraps %v", err, wantErr)
	}
}
