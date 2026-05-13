package aesgcm

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func freshKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func TestNew_RejectsBadKeySize(t *testing.T) {
	cases := map[string]int{
		"empty": 0,
		"short": 16,
		"long":  33,
		"oddly": 31,
	}
	for name, n := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New(make([]byte, n), nil)
			if !errors.Is(err, ErrKeySize) {
				t.Fatalf("err: got %v want ErrKeySize", err)
			}
		})
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	c, err := New(freshKey(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plaintext := []byte("the quick brown fox jumps over the lazy dog")
	ct, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := c.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round trip diverged: got %q want %q", got, plaintext)
	}
}

func TestEncrypt_FreshNoncePerCall(t *testing.T) {
	c, err := New(freshKey(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plaintext := []byte("same input")
	a, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt a: %v", err)
	}
	b, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt b: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatalf("two encrypts of the same plaintext produced identical output — nonce not refreshed")
	}
	if bytes.Equal(a[:nonceSize], b[:nonceSize]) {
		t.Fatalf("nonce reused across calls: %x", a[:nonceSize])
	}
}

func TestEncrypt_RejectsEmptyPlaintext(t *testing.T) {
	c, err := New(freshKey(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Encrypt(nil); err == nil {
		t.Fatal("Encrypt(nil) returned no error")
	}
}

func TestEncrypt_RandReaderError(t *testing.T) {
	c, err := New(freshKey(t), failingReader{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Encrypt([]byte("anything")); err == nil {
		t.Fatal("Encrypt with failing reader returned no error")
	}
}

type failingReader struct{}

func (failingReader) Read(_ []byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestDecrypt_RejectsShortInput(t *testing.T) {
	c, err := New(freshKey(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := [][]byte{nil, {}, make([]byte, nonceSize), make([]byte, nonceSize-1)}
	for _, in := range cases {
		if _, err := c.Decrypt(in); !errors.Is(err, ErrShortCiphertext) {
			t.Fatalf("len=%d err=%v want ErrShortCiphertext", len(in), err)
		}
	}
}

func TestDecrypt_RejectsTamperedTag(t *testing.T) {
	c, err := New(freshKey(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ct, err := c.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Flip the last byte (inside the auth tag region).
	ct[len(ct)-1] ^= 0x01
	if _, err := c.Decrypt(ct); err == nil {
		t.Fatal("Decrypt accepted tampered ciphertext")
	}
}

func TestDecrypt_RejectsTamperedBody(t *testing.T) {
	c, err := New(freshKey(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ct, err := c.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Flip a byte in the ciphertext body (just past the nonce).
	ct[nonceSize] ^= 0x01
	if _, err := c.Decrypt(ct); err == nil {
		t.Fatal("Decrypt accepted tampered ciphertext body")
	}
}

func TestDecrypt_RejectsWrongKey(t *testing.T) {
	a, err := New(freshKey(t), nil)
	if err != nil {
		t.Fatalf("New a: %v", err)
	}
	b, err := New(freshKey(t), nil)
	if err != nil {
		t.Fatalf("New b: %v", err)
	}
	ct, err := a.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := b.Decrypt(ct); err == nil {
		t.Fatal("Decrypt with wrong key did not error")
	}
}
