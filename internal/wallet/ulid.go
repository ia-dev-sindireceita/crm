package wallet

import (
	"crypto/rand"
	"time"
)

// crockfordAlphabet is the Crockford base-32 encoding alphabet used
// by the ULID spec (https://github.com/ulid/spec). Characters that
// look alike (I, L, O, U) are intentionally omitted to reduce
// transcription errors in human-readable contexts.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID generates a ULID (Universally Unique Lexicographically
// Sortable Identifier) using the current wall-clock time and
// crypto/rand. The result is a 26-character Crockford base-32 string.
//
// Layout:
//
//	chars  0–9  : 48-bit millisecond timestamp (most-significant first)
//	chars 10–25 : 80 bits of cryptographic randomness
//
// ULIDs generated within the same millisecond are NOT guaranteed to
// be monotonic; callers that need strict monotonicity within a ms
// must add their own sequence counter.
func NewULID() string {
	return newULIDAt(time.Now())
}

// newULIDAt is the testable entry point; it pins the time source.
func newULIDAt(t time.Time) string {
	ms := uint64(t.UnixMilli())

	var rnd [10]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		panic("wallet: ULID random source failed: " + err.Error())
	}

	var out [26]byte

	// --- timestamp: 10 chars, 5 bits each, MSB first ---
	// We have 48 usable bits in ms. Map them to chars[0..9] where
	// char[0] holds the top 3 bits (bits 47-45) and char[9] holds
	// bits 4-0.
	out[9] = crockfordAlphabet[ms&0x1F]
	out[8] = crockfordAlphabet[(ms>>5)&0x1F]
	out[7] = crockfordAlphabet[(ms>>10)&0x1F]
	out[6] = crockfordAlphabet[(ms>>15)&0x1F]
	out[5] = crockfordAlphabet[(ms>>20)&0x1F]
	out[4] = crockfordAlphabet[(ms>>25)&0x1F]
	out[3] = crockfordAlphabet[(ms>>30)&0x1F]
	out[2] = crockfordAlphabet[(ms>>35)&0x1F]
	out[1] = crockfordAlphabet[(ms>>40)&0x1F]
	out[0] = crockfordAlphabet[(ms>>45)&0x1F]

	// --- randomness: 16 chars, 5 bits each ---
	// 10 bytes (80 bits) packed into 16 × 5-bit groups. We read
	// the 80 bits via two uint64s to avoid messy byte shifts.
	//
	// bits[79..40] live in rnd[0..4] (big-endian), mapped to out[10..17]
	// bits[39.. 0] live in rnd[5..9] (big-endian), mapped to out[18..25]
	hi := uint64(rnd[0])<<32 | uint64(rnd[1])<<24 |
		uint64(rnd[2])<<16 | uint64(rnd[3])<<8 | uint64(rnd[4])
	lo := uint64(rnd[5])<<32 | uint64(rnd[6])<<24 |
		uint64(rnd[7])<<16 | uint64(rnd[8])<<8 | uint64(rnd[9])

	out[17] = crockfordAlphabet[hi&0x1F]
	out[16] = crockfordAlphabet[(hi>>5)&0x1F]
	out[15] = crockfordAlphabet[(hi>>10)&0x1F]
	out[14] = crockfordAlphabet[(hi>>15)&0x1F]
	out[13] = crockfordAlphabet[(hi>>20)&0x1F]
	out[12] = crockfordAlphabet[(hi>>25)&0x1F]
	out[11] = crockfordAlphabet[(hi>>30)&0x1F]
	out[10] = crockfordAlphabet[(hi>>35)&0x1F]

	out[25] = crockfordAlphabet[lo&0x1F]
	out[24] = crockfordAlphabet[(lo>>5)&0x1F]
	out[23] = crockfordAlphabet[(lo>>10)&0x1F]
	out[22] = crockfordAlphabet[(lo>>15)&0x1F]
	out[21] = crockfordAlphabet[(lo>>20)&0x1F]
	out[20] = crockfordAlphabet[(lo>>25)&0x1F]
	out[19] = crockfordAlphabet[(lo>>30)&0x1F]
	out[18] = crockfordAlphabet[(lo>>35)&0x1F]

	return string(out[:])
}
