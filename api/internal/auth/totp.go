package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // RFC 6238 TOTP uses HMAC-SHA1 for authenticator-app compatibility
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters (RFC 6238 defaults understood by every authenticator app).
const (
	TOTPDigits = 6
	TOTPPeriod = 30 * time.Second
	// totpSkew accepts codes from one period before and after now.
	totpSkew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit secret (RFC 4226 recommends ≥ 128).
func NewTOTPSecret() []byte {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return b
}

// EncodeTOTPSecret returns the base32 form shown to users.
func EncodeTOTPSecret(secret []byte) string { return b32.EncodeToString(secret) }

// TOTPStep returns the time step containing t.
func TOTPStep(t time.Time) int64 { return t.Unix() / int64(TOTPPeriod/time.Second) }

// TOTPCode returns the code for secret at step (RFC 4226 dynamic truncation).
func TOTPCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000)
}

// MatchTOTP returns the step whose code equals code within the allowed skew
// around now, or ok=false. Every candidate is compared in constant time.
func MatchTOTP(secret []byte, code string, now time.Time) (step int64, ok bool) {
	cur := TOTPStep(now)
	for s := cur - totpSkew; s <= cur+totpSkew; s++ {
		if subtle.ConstantTimeCompare([]byte(TOTPCode(secret, s)), []byte(code)) == 1 && !ok {
			step, ok = s, true
		}
	}
	return step, ok
}

// OTPAuthURI returns the otpauth:// URI for QR-code enrolment.
func OTPAuthURI(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", EncodeTOTPSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(TOTPDigits))
	q.Set("period", fmt.Sprint(int(TOTPPeriod/time.Second)))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// recoveryAlphabet is lower-case RFC 4648 base32.
const recoveryAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

// RecoveryCodeCount is the number of recovery codes issued at once.
const RecoveryCodeCount = 10

// NewRecoveryCodes returns n codes of the form xxxx-xxxx-xxxx (60 bits each).
func NewRecoveryCodes(n int) []string {
	out := make([]string, 0, n)
	buf := make([]byte, 12)
	for range n {
		if _, err := rand.Read(buf); err != nil {
			panic("crypto/rand failed: " + err.Error())
		}
		var sb strings.Builder
		for i, b := range buf {
			if i > 0 && i%4 == 0 {
				sb.WriteByte('-')
			}
			sb.WriteByte(recoveryAlphabet[b&31])
		}
		out = append(out, sb.String())
	}
	return out
}

// NormalizeRecoveryCode lower-cases a code and removes dashes and spaces.
func NormalizeRecoveryCode(code string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(code)))
}

// RecoveryCodeHash is the stored digest of a recovery code. Codes carry 60
// random bits and are single use, so a fast hash is adequate; the user ID
// is mixed in so equal codes of different users never share a digest.
func RecoveryCodeHash(userID, code string) string {
	return digest("recovery:" + userID + ":" + NormalizeRecoveryCode(code))
}
