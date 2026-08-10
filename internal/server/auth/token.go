package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// NewToken returns a fresh 32-byte random token as base64url cleartext plus
// its sha256. The cleartext goes to the client (session cookie or CSRF
// token); only the hash may be stored.
func NewToken() (cleartext string, hash []byte, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}
	cleartext = base64.RawURLEncoding.EncodeToString(raw)
	return cleartext, HashToken(cleartext), nil
}

// HashToken returns the sha256 of a token's cleartext form.
func HashToken(cleartext string) []byte {
	sum := sha256.Sum256([]byte(cleartext))
	return sum[:]
}
