package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidUUID reports whether s is a lowercase canonical UUID string.
func ValidUUID(s string) bool { return uuidPattern.MatchString(s) }

// Token returns a 256-bit cryptographically random hex identifier.
func Token() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("crypto/rand failed: %w", err))
	}
	return hex.EncodeToString(b[:])
}

// SessionUUID generates a random RFC 4122 version 4 UUID.
func SessionUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("crypto/rand failed: %w", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// Digest returns the lowercase hex SHA-256 digest of value.
func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Decoder constructs a size-limited, strict JSON decoder that rejects unknown fields.
func Decoder(r io.Reader) *json.Decoder {
	d := json.NewDecoder(io.LimitReader(r, MaxMessage))
	d.DisallowUnknownFields()
	return d
}

// Decode decodes a single JSON value using the strict Decoder.
func Decode(r io.Reader, value any) error {
	return Decoder(r).Decode(value)
}

// DecodeDocument decodes exactly one JSON value and rejects trailing tokens.
func DecodeDocument(r io.Reader, value any) error {
	d := Decoder(r)
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}
