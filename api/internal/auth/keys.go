package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/clock"
)

// AlgEdDSA is the only JWT signing algorithm accepted.
const AlgEdDSA = "EdDSA"

// Key repository errors.
var (
	ErrKeyNotFound = errors.New("auth: signing key not found")
	ErrKeyExists   = errors.New("auth: an active signing key already exists")
)

// StoredKey is a signing key as persisted. The private key is sealed with
// the master key; the public key is stored in clear for verification.
type StoredKey struct {
	Id                string
	WorkspaceId       string
	Algorithm         string
	PublicKey         string // base64 (std) Ed25519 public key
	PrivateCiphertext string // secrets envelope bound to SigningKeyContext(ID)
	Active            bool
	CreatedAt         time.Time
	// RetiredAt is set when a rotation replaced this key. Tokens it signed
	// verify until VerifyUntil (the access-token lifetime plus margins).
	RetiredAt   *time.Time
	VerifyUntil *time.Time
}

// KeyRepository persists signing keys.
type KeyRepository interface {
	ActiveKey(ctx context.Context, workspaceID string) (StoredKey, error)
	KeyByID(ctx context.Context, workspaceID, id string) (StoredKey, error)
	// InsertKey stores a new active key and its audit event; it returns
	// ErrKeyExists when another process created an active key first.
	InsertKey(ctx context.Context, k StoredKey, ev audit.Event) error
	// RotateKey retires the active key oldID (keeping it verifiable until
	// verifyUntil) and stores next as the active key, atomically with ev.
	// It returns ErrKeyNotFound when oldID is no longer the active key.
	RotateKey(ctx context.Context, workspaceID, oldID string, next StoredKey, now, verifyUntil time.Time, ev audit.Event) error
}

// activeKeyTTL is how long a process signs with its cached active key
// before re-reading it, so a rotation done by another process (or the CLI)
// takes effect everywhere within this interval.
const activeKeyTTL = time.Minute

// RotationOverlap is how long a retired key must keep verifying: a process
// may still sign with it for activeKeyTTL after the rotation, and those
// tokens live for accessTTL (plus the verifier's 30 s leeway).
func RotationOverlap(accessTTL time.Duration) time.Duration {
	return accessTTL + activeKeyTTL + time.Minute
}

// Sealer encrypts and decrypts with the master key.
type Sealer interface {
	Seal(plaintext []byte, context string) (string, error)
	Open(ciphertext, context string) ([]byte, error)
}

// SigningKeyContext binds a sealed private key to its key ID.
func SigningKeyContext(kid string) string { return "auth.jwtSigningKey:" + kid }

var kidPattern = regexp.MustCompile(`^ed[0-9a-f]{16}$`)

func keyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "ed" + hex.EncodeToString(sum[:8])
}

type signingKey struct {
	id       string
	priv     ed25519.PrivateKey
	loadedAt time.Time
}

// verifyKey is a cached verification key. Entries are re-read after
// activeKeyTTL so a retirement made by another process is noticed.
type verifyKey struct {
	pub      ed25519.PublicKey
	until    *time.Time // nil: active key, no retirement deadline
	cachedAt time.Time
}

// KeyManager loads the persisted signing key, creating it exactly once per
// installation. Keys are never regenerated on boot.
type KeyManager struct {
	repo   KeyRepository
	sealer Sealer
	clock  clock.Clock

	mu      sync.Mutex
	active  map[string]*signingKey // by workspace
	public  map[string]verifyKey   // by workspace/kid
	missing map[string]time.Time   // negative cache by workspace/kid
}

// NewKeyManager returns a key manager.
func NewKeyManager(repo KeyRepository, sealer Sealer, clk clock.Clock) *KeyManager {
	return &KeyManager{repo: repo, sealer: sealer, clock: clk,
		active: map[string]*signingKey{}, public: map[string]verifyKey{}, missing: map[string]time.Time{}}
}

// signing returns the workspace's active signing key, creating and
// persisting it on first use.
func (m *KeyManager) signing(ctx context.Context, workspaceID string) (*signingKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k := m.active[workspaceID]; k != nil && m.clock.Now().Sub(k.loadedAt) < activeKeyTTL {
		return k, nil
	}
	stored, err := m.repo.ActiveKey(ctx, workspaceID)
	if errors.Is(err, ErrKeyNotFound) {
		stored, err = m.create(ctx, workspaceID)
	}
	if err != nil {
		return nil, err
	}
	plain, err := m.sealer.Open(stored.PrivateCiphertext, SigningKeyContext(stored.Id))
	if err != nil {
		return nil, fmt.Errorf("open signing key %s: %w", stored.Id, err)
	}
	if len(plain) != ed25519.SeedSize {
		return nil, fmt.Errorf("signing key %s has an invalid length", stored.Id)
	}
	k := &signingKey{id: stored.Id, priv: ed25519.NewKeyFromSeed(plain), loadedAt: m.clock.Now()}
	m.active[workspaceID] = k
	m.public[workspaceID+"/"+k.id] = verifyKey{pub: k.priv.Public().(ed25519.PublicKey), cachedAt: k.loadedAt}
	return k, nil
}

// newStoredKey generates a key pair and seals its private seed.
func (m *KeyManager) newStoredKey(workspaceID string) (StoredKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return StoredKey{}, err
	}
	kid := keyID(pub)
	sealed, err := m.sealer.Seal(priv.Seed(), SigningKeyContext(kid))
	if err != nil {
		return StoredKey{}, err
	}
	return StoredKey{Id: kid, WorkspaceId: workspaceID, Algorithm: AlgEdDSA,
		PublicKey: base64.StdEncoding.EncodeToString(pub), PrivateCiphertext: sealed, Active: true, CreatedAt: m.clock.Now()}, nil
}

// Rotation is the result of a signing-key rotation.
type Rotation struct {
	NewKeyId    string
	OldKeyId    string // empty when no key existed yet
	VerifyUntil time.Time
}

// Rotate makes a new active signing key. The previous key stops signing
// (other processes notice within a minute) and keeps verifying tokens for
// overlap, which callers set to at least the access-token lifetime plus
// the active-key cache interval, so no valid token is rejected.
func (m *KeyManager) Rotate(ctx context.Context, workspaceID string, overlap time.Duration, actor audit.Actor) (Rotation, error) {
	if overlap < activeKeyTTL {
		return Rotation{}, fmt.Errorf("rotation overlap %s is shorter than the active-key cache interval %s", overlap, activeKeyTTL)
	}
	old, err := m.repo.ActiveKey(ctx, workspaceID)
	if errors.Is(err, ErrKeyNotFound) {
		k, err := m.create(ctx, workspaceID)
		return Rotation{NewKeyId: k.Id}, err
	}
	if err != nil {
		return Rotation{}, err
	}
	next, err := m.newStoredKey(workspaceID)
	if err != nil {
		return Rotation{}, err
	}
	now := m.clock.Now()
	until := now.Add(overlap)
	ev := audit.New(ctx, actor, audit.SigningKeyRotated, "signingKey/"+next.Id, now)
	ev.WorkspaceId = workspaceID
	ev.Reason = "replaces " + old.Id + "; previous key verifies until " + until.UTC().Format(time.RFC3339)
	if err := m.repo.RotateKey(ctx, workspaceID, old.Id, next, now, until, ev); err != nil {
		return Rotation{}, err
	}
	m.mu.Lock()
	delete(m.active, workspaceID)
	delete(m.public, workspaceID+"/"+old.Id)
	m.mu.Unlock()
	return Rotation{NewKeyId: next.Id, OldKeyId: old.Id, VerifyUntil: until}, nil
}

func (m *KeyManager) create(ctx context.Context, workspaceID string) (StoredKey, error) {
	k, err := m.newStoredKey(workspaceID)
	if err != nil {
		return StoredKey{}, err
	}
	kid, now := k.Id, k.CreatedAt
	ev := audit.New(ctx, audit.System(audit.SystemSigningKeys, "token signing"), audit.SigningKeyCreated, "signingKey/"+kid, now)
	ev.WorkspaceId = workspaceID
	err = m.repo.InsertKey(ctx, k, ev)
	if errors.Is(err, ErrKeyExists) {
		return m.repo.ActiveKey(ctx, workspaceID)
	}
	return k, err
}

// verificationKey returns the public key for kid. Unknown key IDs are
// cached negatively for a minute so forged tokens cannot hammer MongoDB.
func (m *KeyManager) verificationKey(ctx context.Context, workspaceID, kid string) (ed25519.PublicKey, error) {
	if !kidPattern.MatchString(kid) {
		return nil, ErrKeyNotFound
	}
	cacheKey := workspaceID + "/" + kid
	m.mu.Lock()
	now := m.clock.Now()
	if vk, ok := m.public[cacheKey]; ok && now.Sub(vk.cachedAt) < activeKeyTTL {
		m.mu.Unlock()
		if vk.until != nil && !now.Before(*vk.until) {
			return nil, ErrKeyNotFound
		}
		return vk.pub, nil
	}
	if until, ok := m.missing[cacheKey]; ok && m.clock.Now().Before(until) {
		m.mu.Unlock()
		return nil, ErrKeyNotFound
	}
	m.mu.Unlock()

	stored, err := m.repo.KeyByID(ctx, workspaceID, kid)
	m.mu.Lock()
	defer m.mu.Unlock()
	if errors.Is(err, ErrKeyNotFound) {
		if len(m.missing) > 1000 {
			clear(m.missing)
		}
		m.missing[cacheKey] = m.clock.Now().Add(time.Minute)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(stored.PublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize || stored.Algorithm != AlgEdDSA {
		return nil, fmt.Errorf("signing key %s is malformed", kid)
	}
	pub := ed25519.PublicKey(raw)
	until := stored.VerifyUntil
	if !stored.Active && until == nil {
		until = &time.Time{} // retired without a deadline: never verifies
	}
	if stored.Active {
		until = nil
	}
	m.public[cacheKey] = verifyKey{pub: pub, until: until, cachedAt: m.clock.Now()}
	if until != nil && !m.clock.Now().Before(*until) {
		return nil, ErrKeyNotFound
	}
	return pub, nil
}
