package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/secrets"
)

type memKeys struct {
	mu      sync.Mutex
	keys    map[string]StoredKey
	inserts int
}

func (m *memKeys) ActiveKey(_ context.Context, ws string) (StoredKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.keys {
		if k.WorkspaceId == ws && k.Active {
			return k, nil
		}
	}
	return StoredKey{}, ErrKeyNotFound
}

func (m *memKeys) KeyByID(_ context.Context, ws, id string) (StoredKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok || k.WorkspaceId != ws {
		return StoredKey{}, ErrKeyNotFound
	}
	return k, nil
}

func (m *memKeys) InsertKey(_ context.Context, k StoredKey, _ audit.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inserts++
	m.keys[k.Id] = k
	return nil
}

func (m *memKeys) RotateKey(_ context.Context, ws, oldID string, next StoredKey, now, until time.Time, _ audit.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.keys[oldID]
	if !ok || !old.Active || old.WorkspaceId != ws {
		return ErrKeyNotFound
	}
	old.Active, old.RetiredAt, old.VerifyUntil = false, &now, &until
	m.keys[oldID] = old
	m.keys[next.Id] = next
	return nil
}

func newTestTokens(t *testing.T) (*Tokens, *memKeys, *clock.Manual, *secrets.Keyring) {
	t.Helper()
	mk := make([]byte, secrets.KeySize)
	rand.Read(mk)
	kr, _ := secrets.NewKeyring(mk)
	repo := &memKeys{keys: map[string]StoredKey{}}
	clk := clock.NewManual(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	return NewTokens(NewKeyManager(repo, kr, clk), clk), repo, clk, kr
}

func TestTokenIssueVerify(t *testing.T) {
	tok, repo, clk, kr := newTestTokens(t)
	ctx := context.Background()
	raw, err := tok.Issue(ctx, "ws1", "user1", "sess1", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c, err := tok.Verify(ctx, "ws1", raw)
	if err != nil || c.Subject != "user1" || c.SessionId != "sess1" {
		t.Fatalf("verify: %+v %v", c, err)
	}
	if _, err := tok.Issue(ctx, "ws1", "user2", "sess2", time.Minute); err != nil || repo.inserts != 1 {
		t.Fatalf("signing key regenerated: inserts=%d", repo.inserts)
	}
	// The private key is stored sealed under the master key.
	for _, k := range repo.keys {
		if !strings.HasPrefix(k.PrivateCiphertext, "v1:"+kr.ActiveKeyID()+":") || k.Algorithm != AlgEdDSA {
			t.Fatalf("stored key: %+v", k)
		}
	}
	// A new manager over the same store reuses the persisted key.
	tok2 := NewTokens(NewKeyManager(repo, kr, clk), clk)
	if _, err := tok2.Verify(ctx, "ws1", raw); err != nil {
		t.Fatalf("restart verify: %v", err)
	}
	if _, err := tok2.Issue(ctx, "ws1", "user3", "sess3", time.Minute); err != nil || repo.inserts != 1 {
		t.Fatal("restart regenerated the signing key")
	}

	// Wrong issuer (another workspace) and expiry.
	if _, err := tok.Verify(ctx, "ws2", raw); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("other workspace: %v", err)
	}
	clk.Advance(11 * time.Minute)
	if _, err := tok.Verify(ctx, "ws1", raw); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired: %v", err)
	}
}

func TestTokenRejectsForgeries(t *testing.T) {
	tok, repo, clk, _ := newTestTokens(t)
	ctx := context.Background()
	good, _ := tok.Issue(ctx, "ws1", "user1", "sess1", 10*time.Minute)
	var kid string
	for id := range repo.keys {
		kid = id
	}
	now := clk.Now()
	base := AccessClaims{RegisteredClaims: jwt.RegisteredClaims{Issuer: Issuer("ws1"), Subject: "user1",
		Audience: jwt.ClaimStrings{Audience}, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}, SessionId: "sess1"}

	sign := func(method jwt.SigningMethod, key any, claims AccessClaims, hdr map[string]any) string {
		t.Helper()
		j := jwt.NewWithClaims(method, claims)
		for k, v := range hdr {
			j.Header[k] = v
		}
		s, err := j.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	pub, _ := tok.keys.verificationKey(ctx, "ws1", kid)

	wrongAud := base
	wrongAud.Audience = jwt.ClaimStrings{"someone-else"}
	noExp := base
	noExp.ExpiresAt = nil
	noSid := base
	noSid.SessionId = ""

	cases := map[string]string{
		"alg none":            sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, base, map[string]any{"kid": kid, "typ": "at+jwt"}),
		"HS256 with pubkey":   sign(jwt.SigningMethodHS256, []byte(pub), base, map[string]any{"kid": kid, "typ": "at+jwt"}),
		"foreign key":         sign(jwt.SigningMethodEdDSA, otherPriv, base, map[string]any{"kid": kid, "typ": "at+jwt"}),
		"unknown kid":         sign(jwt.SigningMethodEdDSA, otherPriv, base, map[string]any{"kid": "ed0000000000000000", "typ": "at+jwt"}),
		"wrong audience":      sign(jwt.SigningMethodEdDSA, otherPriv, wrongAud, map[string]any{"kid": kid, "typ": "at+jwt"}),
		"missing typ":         sign(jwt.SigningMethodEdDSA, otherPriv, base, map[string]any{"kid": kid}),
		"no expiry":           sign(jwt.SigningMethodEdDSA, otherPriv, noExp, map[string]any{"kid": kid, "typ": "at+jwt"}),
		"no session":          sign(jwt.SigningMethodEdDSA, otherPriv, noSid, map[string]any{"kid": kid, "typ": "at+jwt"}),
		"tampered payload":    good[:strings.Index(good, ".")+1] + "eyJzdWIiOiJ4In0" + good[strings.LastIndex(good, "."):],
		"garbage":             "a.b.c",
		"refresh token shape": "dGhpcyBpcyBub3QgYSBqd3Q",
	}
	for name, raw := range cases {
		if _, err := tok.Verify(ctx, "ws1", raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPasswordsAdapter(t *testing.T) {
	p, err := NewPasswords(Argon2Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := p.Hash("correct horse battery")
	if p.Verify("correct horse battery", h) != nil || p.Verify("wrong", h) == nil {
		t.Fatal("verify")
	}
	if !errors.Is(p.Verify("anything", ""), ErrMismatch) || !errors.Is(p.Verify("x", "$bogus"), ErrMismatch) {
		t.Fatal("empty or malformed hash must fail closed")
	}
}

func TestRotationKeepsOldTokensUntilOverlapEnds(t *testing.T) {
	tok, repo, clk, _ := newTestTokens(t)
	ctx := context.Background()
	ttl := 10 * time.Minute
	old, err := tok.Issue(ctx, "ws1", "user1", "sess1", ttl)
	if err != nil {
		t.Fatal(err)
	}
	rot, err := tok.keys.Rotate(ctx, "ws1", RotationOverlap(ttl), audit.Operator(audit.SystemCLI, "test"))
	if err != nil || rot.OldKeyId == "" || rot.NewKeyId == rot.OldKeyId {
		t.Fatalf("rotate: %+v %v", rot, err)
	}
	fresh, err := tok.Issue(ctx, "ws1", "user1", "sess1", ttl)
	if err != nil {
		t.Fatal(err)
	}
	if kid := headerKID(t, fresh); kid != rot.NewKeyId {
		t.Fatalf("new tokens signed with %s, want %s", kid, rot.NewKeyId)
	}
	if _, err := tok.Verify(ctx, "ws1", old); err != nil {
		t.Fatalf("old token rejected right after rotation: %v", err)
	}
	clk.Advance(ttl - time.Minute)
	if _, err := tok.Verify(ctx, "ws1", old); err != nil {
		t.Fatalf("old token rejected before expiry: %v", err)
	}
	// After the overlap the retired key no longer verifies anything.
	clk.Advance(RotationOverlap(ttl))
	if _, err := tok.keys.verificationKey(ctx, "ws1", rot.OldKeyId); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("retired key still verifies after overlap: %v", err)
	}
	if n := len(repo.keys); n != 2 {
		t.Fatalf("keys stored: %d", n)
	}
	if _, err := tok.keys.Rotate(ctx, "ws1", time.Second, audit.Operator(audit.SystemCLI, "test")); err == nil {
		t.Fatal("overlap shorter than the cache interval accepted")
	}
}

func headerKID(t *testing.T, raw string) string {
	t.Helper()
	tk, _, err := jwt.NewParser().ParseUnverified(raw, &AccessClaims{})
	if err != nil {
		t.Fatal(err)
	}
	kid, _ := tk.Header["kid"].(string)
	return kid
}
