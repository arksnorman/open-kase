// Package secrets encrypts database-held credentials with AES-256-GCM under
// a master key that is supplied from a file outside MongoDB.
//
// Ciphertexts carry the key ID that sealed them so that a future staged key
// rotation can keep old keys for decryption only.
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is the master key length in bytes (AES-256).
const KeySize = 32

const envelopeVersion = "v1"

var (
	// ErrUnknownKey means the ciphertext was sealed with a key that is not loaded.
	ErrUnknownKey = errors.New("secrets: ciphertext key is not available")
	// ErrDecrypt means authentication failed: wrong key, wrong context or tampering.
	ErrDecrypt = errors.New("secrets: decryption failed")
	// ErrMalformed means the ciphertext envelope is not recognised.
	ErrMalformed = errors.New("secrets: malformed ciphertext")
)

// Keyring holds the active master key and any decrypt-only previous keys.
type Keyring struct {
	active string
	keys   map[string]cipher.AEAD
}

// NewKeyring builds a keyring whose active key is key.
func NewKeyring(key []byte) (*Keyring, error) {
	kr := &Keyring{keys: map[string]cipher.AEAD{}}
	id, err := kr.add(key)
	if err != nil {
		return nil, err
	}
	kr.active = id
	return kr, nil
}

// AddDecryptOnly registers an older key for decrypting existing ciphertexts.
func (k *Keyring) AddDecryptOnly(key []byte) error {
	_, err := k.add(key)
	return err
}

func (k *Keyring) add(key []byte) (string, error) {
	if len(key) != KeySize {
		return "", fmt.Errorf("secrets: master key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	id := KeyID(key)
	k.keys[id] = aead
	return id, nil
}

// ActiveKeyID returns the ID of the key used for new encryptions.
func (k *Keyring) ActiveKeyID() string { return k.active }

// KeyID derives a stable, non-secret identifier for a key.
func KeyID(key []byte) string {
	sum := sha256.Sum256(append([]byte("open-kase/master-key-id/v1:"), key...))
	return "mk" + hex.EncodeToString(sum[:8])
}

// Seal encrypts plaintext bound to context (additional authenticated data,
// e.g. "settings.smtp.password"). The same context is required to open it.
func (k *Keyring) Seal(plaintext []byte, context string) (string, error) {
	aead := k.keys[k.active]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, plaintext, aad(k.active, context))
	return envelopeVersion + ":" + k.active + ":" + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open decrypts a ciphertext produced by Seal with the same context.
func (k *Keyring) Open(ciphertext, context string) ([]byte, error) {
	parts := strings.SplitN(ciphertext, ":", 3)
	if len(parts) != 3 || parts[0] != envelopeVersion {
		return nil, ErrMalformed
	}
	aead, ok := k.keys[parts[1]]
	if !ok {
		return nil, ErrUnknownKey
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(raw) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrMalformed
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], aad(parts[1], context))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}

func aad(keyID, context string) []byte {
	return []byte(envelopeVersion + "|" + keyID + "|" + context)
}

// Key-check support: a sealed known value stored with the installation lets
// the server prove at startup that it holds the right master key.
const keyCheckContext = "installation.keyCheck"

var keyCheckPlaintext = []byte("open-kase master key check v1")

// NewKeyCheck returns a ciphertext that VerifyKeyCheck accepts with this key.
func (k *Keyring) NewKeyCheck() (string, error) {
	return k.Seal(keyCheckPlaintext, keyCheckContext)
}

// VerifyKeyCheck confirms that check was sealed by a key in this keyring.
func (k *Keyring) VerifyKeyCheck(check string) error {
	plain, err := k.Open(check, keyCheckContext)
	if err != nil {
		return err
	}
	if !bytes.Equal(plain, keyCheckPlaintext) {
		return ErrDecrypt
	}
	return nil
}

// LoadKeyFile reads a base64-encoded 32-byte master key. The file must be a
// regular file that is not readable by others and not writable by its group
// or others.
func LoadKeyFile(path string) (*Keyring, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("master key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("master key file %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o027 != 0 {
		return nil, fmt.Errorf("master key file %s has unsafe permissions %#o; use 0600 or 0400 (0640/0440 for a dedicated group)", path, perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("master key file: %w", err)
	}
	key, err := decodeKey(string(bytes.TrimSpace(raw)))
	if err != nil {
		return nil, fmt.Errorf("master key file %s: %w", path, err)
	}
	return NewKeyring(key)
}

func decodeKey(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(s); err == nil {
			if len(key) != KeySize {
				return nil, fmt.Errorf("key must decode to %d bytes, got %d", KeySize, len(key))
			}
			return key, nil
		}
	}
	return nil, errors.New("key is not valid base64")
}

// CreateKeyFile generates a new random master key at path with mode 0600.
// It never overwrites an existing file. The parent directory is created
// with mode 0700 when missing.
func CreateKeyFile(path string) (*Keyring, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create master key directory: %w", err)
	}
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create master key file: %w", err)
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("write master key file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return NewKeyring(key)
}

// EnsureKeyFile loads the key at path, or creates it when the file does not
// exist. created reports whether a new key was generated.
func EnsureKeyFile(path string) (kr *Keyring, created bool, err error) {
	kr, err = LoadKeyFile(path)
	if err == nil {
		return kr, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	kr, err = CreateKeyFile(path)
	if errors.Is(err, fs.ErrExist) { // lost a creation race; load the winner's key
		kr, err = LoadKeyFile(path)
		return kr, false, err
	}
	return kr, err == nil, err
}
