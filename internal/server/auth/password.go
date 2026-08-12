// Package auth provides password hashing and session-token primitives shared
// by the dashboard HTTP API and the polarbeam-server admin CLI.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (OWASP-recommended profile: 19 MiB, 2 iterations,
// 1 lane). Stored hashes carry their own parameters, so these can change
// without invalidating existing users.
const (
	argonMemoryKiB = 19456
	argonTime      = 2
	argonThreads   = 1
	argonSaltLen   = 16
	argonKeyLen    = 32
)

// MinPasswordLen is the minimum length for operator-chosen passwords,
// enforced by both the CLI's `user add` and the dashboard's self-service
// password change. Server-generated passwords (24 base64url chars) clear
// it trivially.
const MinPasswordLen = 8

// DummyHash is a valid hash of an unguessable throwaway password. Login
// verifies against it when the username does not exist so that response
// timing does not reveal which usernames are real.
const DummyHash = "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$oIWm1DDXsD1L7ZS1RiCbcaCTF645MK2NX4mciZuUUyU"

// HashPassword returns a PHC-encoded argon2id hash of pw.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether pw matches the PHC-encoded hash. Parameters
// come from the stored string, so hashes created under older parameter
// choices keep verifying. A malformed hash is an error, never a silent false.
func VerifyPassword(pw, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// "" / "argon2id" / "v=19" / "m=..,t=..,p=.." / salt / hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, fmt.Errorf("malformed argon2id hash (%d fields)", len(parts))
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, fmt.Errorf("malformed argon2id version %q", parts[2])
	}
	if version != argon2.Version {
		return false, fmt.Errorf("unsupported argon2 version %d", version)
	}
	var mem, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time, &threads); err != nil {
		return false, fmt.Errorf("malformed argon2id parameters %q", parts[3])
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("malformed argon2id salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("malformed argon2id hash: %w", err)
	}
	got := argon2.IDKey([]byte(pw), salt, time, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
