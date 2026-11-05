package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/product"
)

// Audience is the JWT audience for API access tokens.
const Audience = product.Slug + "-api"

// Issuer returns the JWT issuer for a workspace.
func Issuer(workspaceID string) string { return "urn:" + product.Slug + ":workspace:" + workspaceID }

// AccessClaims are the claims of an access token. They carry identity and
// session only, never permissions: authorization is evaluated per request.
type AccessClaims struct {
	jwt.RegisteredClaims
	SessionId string `json:"sid"`
}

// Token errors.
var (
	ErrTokenExpired = errors.New("auth: access token expired")
	ErrTokenInvalid = errors.New("auth: access token invalid")
)

// Tokens issues and verifies EdDSA access tokens.
type Tokens struct {
	keys  *KeyManager
	clock clock.Clock
}

// NewTokens returns a token issuer/verifier.
func NewTokens(keys *KeyManager, clk clock.Clock) *Tokens { return &Tokens{keys: keys, clock: clk} }

// Issue signs an access token for userID/sessionID valid for ttl.
func (t *Tokens) Issue(ctx context.Context, workspaceID, userID, sessionID string, ttl time.Duration) (string, error) {
	key, err := t.keys.signing(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	now := t.clock.Now()
	jti := make([]byte, 12)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	claims := AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: Issuer(workspaceID), Subject: userID, Audience: jwt.ClaimStrings{Audience},
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)), ID: hex.EncodeToString(jti),
		},
		SessionId: sessionID,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = key.id
	tok.Header["typ"] = "at+jwt"
	return tok.SignedString(key.priv)
}

// Verify checks the signature (EdDSA only, known key ID), issuer, audience,
// expiry and required claims. It does not check session or user status.
func (t *Tokens) Verify(ctx context.Context, workspaceID, raw string) (AccessClaims, error) {
	var claims AccessClaims
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{AlgEdDSA}),
		jwt.WithIssuer(Issuer(workspaceID)),
		jwt.WithAudience(Audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(30*time.Second),
		jwt.WithTimeFunc(t.clock.Now),
	)
	_, err := parser.ParseWithClaims(raw, &claims, func(tok *jwt.Token) (any, error) {
		if typ, _ := tok.Header["typ"].(string); typ != "at+jwt" {
			return nil, ErrTokenInvalid
		}
		kid, _ := tok.Header["kid"].(string)
		return t.keys.verificationKey(ctx, workspaceID, kid)
	})
	if errors.Is(err, jwt.ErrTokenExpired) {
		return AccessClaims{}, ErrTokenExpired
	}
	if err != nil {
		return AccessClaims{}, ErrTokenInvalid
	}
	if claims.Subject == "" || claims.SessionId == "" {
		return AccessClaims{}, ErrTokenInvalid
	}
	return claims, nil
}
