package auth

import (
	"errors"
	"strings"
	"testing"
)

var fast = Argon2Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery", fast)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("hash format %q", h)
	}
	if rehash, err := VerifyPassword("correct horse battery", h, fast); err != nil || rehash {
		t.Fatalf("verify: rehash=%v err=%v", rehash, err)
	}
	if _, err := VerifyPassword("wrong password!!", h, fast); !errors.Is(err, ErrMismatch) {
		t.Fatalf("wrong password: %v", err)
	}
	if rehash, err := VerifyPassword("correct horse battery", h, DefaultArgon2Params); err != nil || !rehash {
		t.Fatalf("expected rehash with stronger params: %v %v", rehash, err)
	}
	h2, _ := HashPassword("correct horse battery", fast)
	if h == h2 {
		t.Fatal("salts must differ")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	for _, h := range []string{"", "plain", "$bcrypt$x$y$z$w", "$argon2id$v=19$m=x$a$b"} {
		if _, err := VerifyPassword("p", h, fast); err == nil || errors.Is(err, ErrMismatch) {
			t.Errorf("hash %q: %v", h, err)
		}
	}
}
