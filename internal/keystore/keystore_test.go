package keystore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSealUnseal(t *testing.T) {
	plain := []byte("8f1e…seed\n")
	b, err := Seal(plain, "비밀번호 123")
	if err != nil {
		t.Fatal(err)
	}
	if !Locked(b) || bytes.Contains(b, plain) {
		t.Fatal("sealed file shows its content")
	}
	got, err := Unseal(b, "비밀번호 123")
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("unseal: %v", err)
	}
	if _, err := Unseal(b, "비밀번호 124"); !errors.Is(err, ErrPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := Unseal(b, ""); !errors.Is(err, ErrLocked) {
		t.Fatalf("no password: %v", err)
	}
	b[len(b)-5] ^= 1
	if _, err := Unseal(b, "비밀번호 123"); !errors.Is(err, ErrPassword) {
		t.Fatal("damaged file opened")
	}
	if got, _ := Unseal(plain, ""); !bytes.Equal(got, plain) {
		t.Fatal("plain content changed")
	}
}

// Plain keys move to a password, the password changes, and the old one no
// longer opens anything.
func TestRekey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wallet.key"), []byte("aa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rln.key"), []byte(`{"secret":"bb"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if plain, locked := State(dir); len(plain) != 2 || len(locked) != 0 {
		t.Fatalf("state %v %v", plain, locked)
	}
	if err := Rekey(dir, "", "one"); err != nil {
		t.Fatal(err)
	}
	if plain, locked := State(dir); len(plain) != 0 || len(locked) != 2 {
		t.Fatalf("after locking: %v %v", plain, locked)
	}
	if err := Rekey(dir, "wrong", "two"); err == nil {
		t.Fatal("rekey with a wrong password")
	}
	if err := Rekey(dir, "one", "two"); err != nil {
		t.Fatal(err)
	}
	if Check(dir, "one") == nil || Check(dir, "two") != nil {
		t.Fatal("old password still works or new one does not")
	}
	got, err := Read(filepath.Join(dir, "wallet.key"), "two")
	if err != nil || string(got) != "aa\n" {
		t.Fatalf("content after rekey: %q %v", got, err)
	}
	if err := Write(filepath.Join(dir, "pseudonym.key"), []byte("cc"), "two"); err != nil {
		t.Fatal(err)
	}
	if _, locked := State(dir); len(locked) != 3 {
		t.Fatal("new key not locked")
	}
}
