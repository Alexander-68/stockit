package auth

import (
	"strings"
	"testing"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if !strings.HasPrefix(hash, "argon2id$") {
		t.Fatalf("hash missing argon2id prefix: %q", hash)
	}

	ok, err := VerifyPassword(hash, "correct horse battery staple")
	if err != nil {
		t.Fatalf("verify correct password: %v", err)
	}
	if !ok {
		t.Fatal("VerifyPassword returned false for the correct password")
	}

	ok, err = VerifyPassword(hash, "wrong password")
	if err != nil {
		t.Fatalf("verify wrong password: %v", err)
	}
	if ok {
		t.Fatal("VerifyPassword returned true for a wrong password")
	}
}

func TestHashPasswordProducesUniqueSalts(t *testing.T) {
	a, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	b, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of the same password should differ due to random salts")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	cases := []string{
		"",
		"plaintext",
		"argon2id$1$2$3",                                  // too few segments
		"bcrypt$1$65536$4$c2FsdA$aGFzaA",                  // wrong algorithm
		"argon2id$bad$65536$4$c2FsdA$aGFzaA",              // unparsable time cost
		"argon2id$1$bad$4$c2FsdA$aGFzaA",                  // unparsable memory cost
		"argon2id$1$65536$bad$c2FsdA$aGFzaA",              // unparsable threads
		"argon2id$1$65536$4$!!!notbase64$aGFzaA",          // bad salt
		"argon2id$1$65536$4$c2FsdA$!!!notbase64",          // bad hash
	}
	for _, encoded := range cases {
		ok, err := VerifyPassword(encoded, "anything")
		if err == nil {
			t.Fatalf("VerifyPassword(%q) expected an error", encoded)
		}
		if ok {
			t.Fatalf("VerifyPassword(%q) unexpectedly returned true", encoded)
		}
	}
}
