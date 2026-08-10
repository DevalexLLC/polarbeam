package auth

import (
	"strings"
	"testing"
)

func TestHashVerifyRoundtrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("unexpected PHC prefix: %s", hash)
	}
	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Errorf("verify correct password = %v, %v", ok, err)
	}
	ok, err = VerifyPassword("wrong password", hash)
	if err != nil || ok {
		t.Errorf("verify wrong password = %v, %v", ok, err)
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, _ := HashPassword("same password")
	b, _ := HashPassword("same password")
	if a == b {
		t.Error("two hashes of the same password are identical (salt not random)")
	}
}

func TestVerifyParsesParamsFromHash(t *testing.T) {
	// A hash produced under different (weaker, test-only) parameters must
	// still verify: parameters live in the stored string.
	const legacy = "$argon2id$v=19$m=8,t=1,p=1$c29tZXNhbHQ$WlLL"
	// Not a real match — just confirm it parses rather than erroring.
	if _, err := VerifyPassword("anything", legacy); err != nil {
		t.Errorf("legacy-parameter hash should parse, got %v", err)
	}
}

func TestVerifyMalformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=19456,t=2,p=1$AAAA$BBBB",  // wrong variant
		"$argon2id$v=18$m=19456,t=2,p=1$AAAA$BBBB", // wrong version
		"$argon2id$v=19$m=19456,t=2$AAAA$BBBB",     // missing param
		"$argon2id$v=19$m=19456,t=2,p=1$!!$BBBB",   // bad salt b64
		"$argon2id$v=19$m=19456,t=2,p=1$AAAA$!!",   // bad hash b64
	} {
		if _, err := VerifyPassword("pw", bad); err == nil {
			t.Errorf("VerifyPassword(%q): expected error", bad)
		}
	}
}

func TestDummyHash(t *testing.T) {
	ok, err := VerifyPassword("any guess at all", DummyHash)
	if err != nil {
		t.Fatalf("DummyHash must be well-formed: %v", err)
	}
	if ok {
		t.Error("DummyHash verified true — it must never match")
	}
}

func TestTokens(t *testing.T) {
	clear1, hash1, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	clear2, _, _ := NewToken()
	if clear1 == clear2 {
		t.Error("two tokens are identical")
	}
	if len(hash1) != 32 {
		t.Errorf("hash length = %d, want 32 (sha256)", len(hash1))
	}
	if string(HashToken(clear1)) != string(hash1) {
		t.Error("HashToken(cleartext) != hash returned by NewToken")
	}
}
