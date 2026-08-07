package auth

import "testing"

func TestHashPassword_VerifyPassword_RoundTrips(t *testing.T) {
	t.Parallel()
	encoded, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hashPassword() error: %v", err)
	}

	ok, err := verifyPassword(encoded, "correct horse battery staple")
	if err != nil {
		t.Fatalf("verifyPassword() error: %v", err)
	}
	if !ok {
		t.Error("verifyPassword() = false, want true for correct password")
	}
}

func TestVerifyPassword_WrongPasswordFails(t *testing.T) {
	t.Parallel()
	encoded, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hashPassword() error: %v", err)
	}

	ok, err := verifyPassword(encoded, "wrong password")
	if err != nil {
		t.Fatalf("verifyPassword() error: %v", err)
	}
	if ok {
		t.Error("verifyPassword() = true, want false for wrong password")
	}
}

func TestHashPassword_ProducesDistinctSaltsPerCall(t *testing.T) {
	t.Parallel()
	a, err := hashPassword("same password")
	if err != nil {
		t.Fatalf("hashPassword() error: %v", err)
	}
	b, err := hashPassword("same password")
	if err != nil {
		t.Fatalf("hashPassword() error: %v", err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical — salt is not random")
	}
}
