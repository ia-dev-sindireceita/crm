package hibp

import (
	"bufio"
	"context"
	"crypto/sha1"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
)

//go:embed data/top.sha1
var bundledTopList []byte

// LocalList implements password.PwnedPasswordChecker against the bundled
// top-N SHA-1 set. Used as the fallback when the upstream HIBP API is
// degraded — see ADR 0070 §5 fail-shape.
//
// The set is loaded once at construction so IsPwned is an O(1) map lookup
// on the request path. The default corpus is a small, code-reviewable
// list embedded via go:embed; deploys that need the full HIBP top-100k
// replace the file in data/top.sha1 (same on-disk format).
type LocalList struct {
	hashes map[string]struct{}
}

// NewLocalList parses the bundled top-N file and returns a ready
// LocalList. Returns an error only if the bundled file is malformed —
// which is a build-time failure, not a runtime one.
func NewLocalList() (*LocalList, error) {
	return newLocalListFromBytes(bundledTopList)
}

func newLocalListFromBytes(b []byte) (*LocalList, error) {
	set := make(map[string]struct{}, 256)
	scanner := bufio.NewScanner(strings.NewReader(string(b)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		s := strings.TrimSpace(scanner.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.ToUpper(s)
		if !isSha1Hex(s) {
			return nil, fmt.Errorf("hibp: top.sha1 line %d: %q is not a SHA-1 hex digest", line, s)
		}
		set[s] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("hibp: read bundled list: %w", err)
	}
	return &LocalList{hashes: set}, nil
}

// IsPwned reports whether the SHA-1 of plain is in the bundled corpus.
// The error return is always nil for the in-memory implementation; it is
// kept on the signature so LocalList satisfies the
// password.PwnedPasswordChecker port unchanged.
func (l *LocalList) IsPwned(_ context.Context, plain string) (bool, error) {
	if l == nil || len(l.hashes) == 0 {
		return false, nil
	}
	sum := sha1.Sum([]byte(plain)) //nolint:gosec // SHA-1 is the HIBP corpus key, not a security primitive here.
	hex := strings.ToUpper(hex.EncodeToString(sum[:]))
	_, ok := l.hashes[hex]
	return ok, nil
}

// Size returns the number of entries loaded — useful for sanity tests
// and ops dashboards.
func (l *LocalList) Size() int {
	if l == nil {
		return 0
	}
	return len(l.hashes)
}

func isSha1Hex(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
