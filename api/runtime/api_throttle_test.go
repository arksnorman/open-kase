package runtime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/auth"
)

// burst sends n identical requests at the same time and returns the problem
// codes (or "ok" for 2xx) it got back.
func (h *harness) burst(n int, send func() result) map[string]int {
	h.t.Helper()
	var wg sync.WaitGroup
	codes := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := send()
			if r.Code < 300 {
				codes <- "ok"
				return
			}
			codes <- r.Problem().Code
		}()
	}
	wg.Wait()
	close(codes)
	out := map[string]int{}
	for c := range codes {
		out[c]++
	}
	return out
}

// TestLoginThrottleHoldsUnderConcurrentGuesses sends twenty wrong passwords
// for one account at the same moment. The account budget is five failures
// per window: at most five guesses may be checked against the hash; the rest
// must be refused before any password work (OC-AUTH-02).
func TestLoginThrottleHoldsUnderConcurrentGuesses(t *testing.T) {
	h := newHarness(t)
	h.createUser(h.admin(), "other@example.com", "Other")
	before := auth.ArgonComputations()
	got := h.burst(20, func() result {
		return h.do("POST", "/api/v1/auth/login", map[string]string{"login": adminEmail, "password": "not the password"}, header("Origin", testOrigin))
	})
	hashes := auth.ArgonComputations() - before
	if got["invalidCredentials"] > 5 || got["loginThrottled"] < 15 || hashes > 5 {
		t.Fatalf("concurrent guesses evaluated: %v (%d password hashes), want at most 5", got, hashes)
	}
	// The account is locked for this client for the window; the right
	// password is refused too, and a sign-in by someone else is unaffected.
	h.do("POST", "/api/v1/auth/login", map[string]string{"login": adminEmail, "password": adminPassword}, header("Origin", testOrigin)).expect(429, "loginThrottled")
	h.login("other@example.com", agentPassword)
}

// loginFrom signs in from the client address ip (the connecting address,
// which throttling uses when no trusted proxy is configured).
func (h *harness) loginFrom(ip, login, password string) result {
	h.t.Helper()
	return h.do("POST", "/api/v1/auth/login", map[string]string{"login": login, "password": password},
		header("Origin", testOrigin), func(req *http.Request) { req.RemoteAddr = net.JoinHostPort(ip, "1234") })
}

// TestLoginLockoutIsPerClient: wrong passwords lock an account only for the
// client address that sent them, so nobody can keep an administrator (the
// bootstrap username "admin" is predictable) locked out from everywhere. A
// success clears the client's own counter; unknown identifiers behave the
// same (OC-AUTH-04).
func TestLoginLockoutIsPerClient(t *testing.T) {
	h := newHarness(t)
	const a, b, c = "198.51.100.1", "198.51.100.2", "198.51.100.3"
	for i := 0; i < 5; i++ {
		h.loginFrom(a, "admin", "wrong password").expect(401, "invalidCredentials")
	}
	r := h.loginFrom(a, "admin", adminPassword).expect(429, "loginThrottled")
	if r.Header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After header")
	}
	// The client's limit is the account's, whichever identifier names it.
	h.loginFrom(a, adminEmail, adminPassword).expect(429, "loginThrottled")
	// Another client signs in.
	h.loginFrom(b, "admin", adminPassword).expect(200)
	h.loginFrom(b, adminEmail, adminPassword).expect(200)

	// A success clears the client's counter: four failures, a success and
	// four more failures stay below the limit.
	for round := 0; round < 2; round++ {
		for i := 0; i < 4; i++ {
			h.loginFrom(c, "admin", "wrong password").expect(401, "invalidCredentials")
		}
		h.loginFrom(c, "admin", adminPassword).expect(200)
	}

	// Unknown identifiers: the same limit per client, nothing for others.
	for i := 0; i < 5; i++ {
		h.loginFrom(a, "nobody", "wrong password").expect(401, "invalidCredentials")
	}
	h.loginFrom(a, "nobody", "wrong password").expect(429, "loginThrottled")
	h.loginFrom(b, "nobody", "wrong password").expect(401, "invalidCredentials")
}

// TestLoginLockoutIPv6ClientIsItsNetwork: the addresses of one IPv6 /64 are
// one client, so a host cannot rotate addresses inside its network to get
// past the per-client limit (and reach the account-wide ceiling alone); the
// next /64 is another client (OC-AUTH-04).
func TestLoginLockoutIPv6ClientIsItsNetwork(t *testing.T) {
	h := newHarness(t)
	for i := 1; i <= 5; i++ {
		h.loginFrom(fmt.Sprintf("2001:db8:1:2::%x", i), "admin", "wrong password").expect(401, "invalidCredentials")
	}
	h.loginFrom("2001:db8:1:2::ff", "admin", "wrong password").expect(429, "loginThrottled")
	h.loginFrom("2001:db8:1:2:ffff:ffff:ffff:ffff", "admin", adminPassword).expect(429, "loginThrottled")
	h.loginFrom("2001:db8:1:3::1", "admin", adminPassword).expect(200)
}

// TestLoginLockoutWithoutClientAddress: requests through a proxy that is not
// declared as trusted have no usable client address. They share one client
// per account, limited as before, while clients whose address is known keep
// their own budget (OC-AUTH-04).
func TestLoginLockoutWithoutClientAddress(t *testing.T) {
	h := newHarness(t)
	viaProxy := func(proxy, password string) result {
		return h.do("POST", "/api/v1/auth/login", map[string]string{"login": "admin", "password": password}, header("Origin", testOrigin),
			header("X-Forwarded-For", "203.0.113.50"), func(req *http.Request) { req.RemoteAddr = net.JoinHostPort(proxy, "1234") })
	}
	for i := 0; i < 5; i++ {
		viaProxy("10.0.0.5", "wrong password").expect(401, "invalidCredentials")
	}
	viaProxy("10.0.0.6", adminPassword).expect(429, "loginThrottled")
	h.loginFrom("198.51.100.1", "admin", adminPassword).expect(200)
}

// TestLoginAccountBackstopAcrossClients: guessing spread over many client
// addresses meets the account-wide ceiling of 50 failures per window. Once
// it is reached the account is refused from every address, even with the
// right password. A success neither clears the ceiling (correct sign-ins
// interleaved with the guesses cannot reopen it) nor counts toward it.
// Unknown identifiers reach the same ceiling (OC-AUTH-04).
func TestLoginAccountBackstopAcrossClients(t *testing.T) {
	h := newHarness(t)
	h.createUser(h.admin(), "other@example.com", "Other")
	ip := func(n int) string { return fmt.Sprintf("198.51.100.%d", n) }
	// 49 failures: five from each of nine clients, four from a tenth.
	for n := 1; n <= 10; n++ {
		for i := 0; i < 5 && (n-1)*5+i < 49; i++ {
			h.loginFrom(ip(n), "admin", "wrong password").expect(401, "invalidCredentials")
		}
	}
	// Successes from new clients, by username and by email.
	h.loginFrom(ip(11), "admin", adminPassword).expect(200)
	h.loginFrom(ip(12), adminEmail, adminPassword).expect(200)
	// The fiftieth failure reaches the ceiling.
	h.loginFrom(ip(10), "admin", "wrong password").expect(401, "invalidCredentials")
	r := h.loginFrom(ip(13), "admin", adminPassword).expect(429, "loginThrottled")
	if ra, err := strconv.Atoi(r.Header.Get("Retry-After")); err != nil || ra <= 0 || ra > 15*60 {
		t.Fatalf("Retry-After %q", r.Header.Get("Retry-After"))
	}
	h.loginFrom(ip(13), adminEmail, adminPassword).expect(429, "loginThrottled")
	h.loginFrom(ip(11), "admin", adminPassword).expect(429, "loginThrottled")
	// Other accounts are unaffected.
	h.loginFrom(ip(13), "other@example.com", agentPassword).expect(200)

	// An unknown identifier reaches the same ceiling.
	for n := 1; n <= 10; n++ {
		for i := 0; i < 5; i++ {
			h.loginFrom(fmt.Sprintf("203.0.113.%d", n), "nobody", "wrong password").expect(401, "invalidCredentials")
		}
	}
	h.loginFrom("203.0.113.11", "nobody", "wrong password").expect(429, "loginThrottled")
}

// TestLoginBackstopHoldsUnderConcurrentGuesses sends sixty wrong passwords
// for one account at the same moment, each from its own address: every
// client is within its own budget, so only the account-wide ceiling (50)
// bounds how many guesses reach the password hash, and afterwards the
// account is refused everywhere (OC-AUTH-04 with OC-AUTH-02).
func TestLoginBackstopHoldsUnderConcurrentGuesses(t *testing.T) {
	h := newHarness(t)
	var next atomic.Int32
	before := auth.ArgonComputations()
	got := h.burst(60, func() result {
		return h.loginFrom(fmt.Sprintf("198.51.100.%d", next.Add(1)), "admin", "not the password")
	})
	hashes := auth.ArgonComputations() - before
	// Two counters (identifier and account) are each raised atomically, so
	// in the worst interleaving ten guesses are refused on each: at least 40
	// and at most 50 reach the hash.
	if n := got["invalidCredentials"]; n < 40 || n > 50 || n+got["loginThrottled"] != 60 || hashes > 50 {
		t.Fatalf("concurrent guesses from distinct clients evaluated: %v (%d password hashes), want 40-50", got, hashes)
	}
	h.loginFrom("198.51.100.200", "admin", adminPassword).expect(429, "loginThrottled")
}

// TestMFAThrottleHoldsUnderConcurrentGuesses does the same for the second
// factor: twenty wrong codes for one challenge at once. The challenge allows
// five attempts and the account five code failures per window; a burst must
// not evaluate more than five codes.
func TestMFAThrottleHoldsUnderConcurrentGuesses(t *testing.T) {
	f := newIdentityFixture(t)
	admin := f.admin()
	f.enrol(admin, adminPassword)
	tok := f.passwordStep(adminEmail, adminPassword)
	got := f.burst(20, func() result {
		return f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": tok, "code": "000000"})
	})
	if got["invalidMfaCode"] > 5 || got["ok"] != 0 {
		t.Fatalf("concurrent codes evaluated: %v, want at most 5", got)
	}
	f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": tok, "code": "000000"}).expect(401, "mfaChallengeInvalid")
	f.clk.Advance(auth.TOTPPeriod)
	f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": f.passwordStep(adminEmail, adminPassword), "code": "000000"}).expect(429, "mfaThrottled")
}

// TestPasswordResetRequestsHoldUnderConcurrency: a burst of reset requests
// for one address must not queue more than the three emails per hour the
// limit allows.
func TestPasswordResetRequestsHoldUnderConcurrency(t *testing.T) {
	f := newIdentityFixture(t)
	got := f.burst(12, func() result {
		return f.post("/api/v1/auth/password-reset/request", map[string]any{"email": adminEmail})
	})
	if got["ok"] > 3 || got["resetThrottled"] < 9 {
		t.Fatalf("concurrent reset requests accepted: %v, want at most 3", got)
	}
	n, err := f.rawDB().Collection("accountTokens").CountDocuments(context.Background(), bson.D{{Key: "purpose", Value: "passwordReset"}})
	if err != nil {
		t.Fatal(err)
	}
	if n > 3 {
		t.Fatalf("%d reset tokens issued, want at most 3", n)
	}
}
