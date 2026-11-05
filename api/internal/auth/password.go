// Package auth implements authentication primitives. Sessions and tokens
// arrive in Milestone 2; this file provides password hashing used by
// installation and later by login.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are versioned Argon2id parameters. They are stored in each
// hash (PHC string format) so they can be raised without invalidating
// existing hashes.
type Argon2Params struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultArgon2Params exceed the OWASP minimum (19 MiB, t=2, p=1). Calibrate
// on deployment hardware before raising further.
var DefaultArgon2Params = Argon2Params{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}

// argonSlots bounds concurrent Argon2id computations process-wide. Each
// one allocates Params.Memory (64 MiB by default), so without a bound a burst
// of sign-in attempts could exhaust memory; callers queue for a slot instead.
var argonSlots = make(chan struct{}, max(2, runtime.NumCPU()))

var argonRuns atomic.Int64

// ArgonComputations is the number of Argon2id computations since start
// (diagnostics and tests).
func ArgonComputations() int64 { return argonRuns.Load() }

func argonKey(password, salt []byte, t, m uint32, threads uint8, keyLen uint32) []byte {
	argonSlots <- struct{}{}
	defer func() { <-argonSlots }()
	argonRuns.Add(1)
	return argon2.IDKey(password, salt, t, m, threads, keyLen)
}

// ErrMismatch means the password does not match the hash.
var ErrMismatch = errors.New("auth: password does not match")

// HashPassword returns a PHC-format Argon2id hash.
func HashPassword(password string, p Argon2Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argonKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks password against a PHC Argon2id hash in constant time.
// needsRehash reports whether the hash uses parameters other than current.
func VerifyPassword(password, encoded string, current Argon2Params) (needsRehash bool, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("auth: unsupported password hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("auth: unsupported argon2 version")
	}
	var p Argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return false, errors.New("auth: malformed argon2 parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, errors.New("auth: malformed salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, errors.New("auth: malformed hash")
	}
	got := argonKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, ErrMismatch
	}
	needsRehash = p.Memory != current.Memory || p.Time != current.Time || p.Threads != current.Threads ||
		uint32(len(salt)) != current.SaltLen || uint32(len(want)) != current.KeyLen
	return needsRehash, nil
}
