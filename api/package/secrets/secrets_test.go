package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func TestSealOpenRoundTrip(t *testing.T) {
	kr, err := NewKeyring(testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	ct, err := kr.Seal([]byte("smtp-pass"), "settings.smtp.password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ct, "v1:"+kr.ActiveKeyID()+":") || strings.Contains(ct, "smtp-pass") {
		t.Fatalf("unexpected envelope %q", ct)
	}
	pt, err := kr.Open(ct, "settings.smtp.password")
	if err != nil || string(pt) != "smtp-pass" {
		t.Fatalf("open: %q %v", pt, err)
	}
	ct2, _ := kr.Seal([]byte("smtp-pass"), "settings.smtp.password")
	if ct == ct2 {
		t.Fatal("nonces must differ")
	}
}

func TestOpenRejectsWrongContextKeyAndTampering(t *testing.T) {
	kr, _ := NewKeyring(testKey(1))
	ct, _ := kr.Seal([]byte("x"), "a")
	if _, err := kr.Open(ct, "b"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong context: %v", err)
	}
	other, _ := NewKeyring(testKey(2))
	if _, err := other.Open(ct, "a"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("wrong key: %v", err)
	}
	tampered := ct[:len(ct)-2] + "AA"
	if tampered == ct {
		tampered = ct[:len(ct)-2] + "BB"
	}
	if _, err := kr.Open(tampered, "a"); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, err := kr.Open("garbage", "a"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestDecryptOnlyOldKey(t *testing.T) {
	old, _ := NewKeyring(testKey(1))
	ct, _ := old.Seal([]byte("legacy"), "c")
	kr, _ := NewKeyring(testKey(2))
	if err := kr.AddDecryptOnly(testKey(1)); err != nil {
		t.Fatal(err)
	}
	if pt, err := kr.Open(ct, "c"); err != nil || string(pt) != "legacy" {
		t.Fatalf("old key open: %q %v", pt, err)
	}
	ct2, _ := kr.Seal([]byte("new"), "c")
	if !strings.Contains(ct2, kr.ActiveKeyID()) || kr.ActiveKeyID() == old.ActiveKeyID() {
		t.Fatal("new ciphertexts must use the active key")
	}
}

func TestKeyCheck(t *testing.T) {
	kr, _ := NewKeyring(testKey(1))
	check, err := kr.NewKeyCheck()
	if err != nil {
		t.Fatal(err)
	}
	if err := kr.VerifyKeyCheck(check); err != nil {
		t.Fatalf("verify: %v", err)
	}
	other, _ := NewKeyring(testKey(3))
	if err := other.VerifyKeyCheck(check); err == nil {
		t.Fatal("different key passed key check")
	}
}

func TestKeyFileLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "master.key")
	kr, created, err := EnsureKeyFile(path)
	if err != nil || !created {
		t.Fatalf("create: %v created=%v", err, created)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %#o", info.Mode().Perm())
	}
	again, created, err := EnsureKeyFile(path)
	if err != nil || created || again.ActiveKeyID() != kr.ActiveKeyID() {
		t.Fatalf("reload: %v created=%v", err, created)
	}
	if _, err := CreateKeyFile(path); err == nil {
		t.Fatal("CreateKeyFile overwrote an existing key")
	}
}

func TestLoadKeyFileRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	world := filepath.Join(dir, "world.key")
	os.WriteFile(world, []byte(strings.Repeat("A", 43)+"=\n"), 0o644)
	if _, err := LoadKeyFile(world); err == nil || !strings.Contains(err.Error(), "unsafe permissions") {
		t.Fatalf("world-readable key: %v", err)
	}
	short := filepath.Join(dir, "short.key")
	os.WriteFile(short, []byte("c2hvcnQ=\n"), 0o600)
	if _, err := LoadKeyFile(short); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := LoadKeyFile(filepath.Join(dir, "missing.key")); err == nil {
		t.Fatal("missing key accepted")
	}
	group := filepath.Join(dir, "group.key")
	os.WriteFile(group, []byte(strings.Repeat("A", 43)+"=\n"), 0o640)
	if _, err := LoadKeyFile(group); err != nil {
		t.Fatalf("0640 key rejected: %v", err)
	}
}
