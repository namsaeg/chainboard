// Package keystore keeps secret key files, optionally locked with a
// password.
//
// A locked file is one line: "cbkey1:" + hex of
//
//	salt (16) | Argon2id time (u32) | memory KiB (u32) | nonce (12) | AES-256-GCM(content)
//
// with the AES key derived from the password by Argon2id (RFC 9106). The
// GCM tag makes a wrong password or a damaged file fail loudly instead of
// yielding a wrong key. Unlocked files keep their old plain format, so a
// node can move to a password at any time.
package keystore

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
)

const prefix = "cbkey1:"

// Argon2id cost of a password: about a tenth of a second on a PC, so each
// guess at a stolen file costs the same.
const (
	argonTime   = 3
	argonMemKiB = 64 * 1024
	argonLanes  = 4
)

var (
	// ErrLocked: the file needs a password and none was given.
	ErrLocked = errors.New("this key file is locked with a password")
	// ErrPassword: the password is wrong (or the file is damaged).
	ErrPassword = errors.New("wrong password (or the key file is damaged)")
)

// SecretFiles are the key files of a node directory that a password locks.
// node.key (the network identity, separate from the wallet) stays plain so
// the node can connect before it is unlocked.
var SecretFiles = []string{"wallet.key", "rln.key", "pseudonym.key", "onion.key", "tipkeys.key"}

// Locked reports whether file content is password-locked.
func Locked(b []byte) bool { return bytes.HasPrefix(b, []byte(prefix)) }

func derive(password string, salt []byte, time, mem uint32) []byte {
	return argon2.IDKey([]byte(password), salt, time, mem, argonLanes, 32)
}

// Seal locks content with a password.
func Seal(plain []byte, password string) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(derive(password, salt, argonTime, argonMemKiB))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	head := append(salt, binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, argonTime), argonMemKiB)...)
	head = append(head, nonce...)
	body := gcm.Seal(head, nonce, plain, []byte(prefix))
	return []byte(prefix + hex.EncodeToString(body) + "\n"), nil
}

// Unseal opens locked content; plain content is returned as it is.
func Unseal(b []byte, password string) ([]byte, error) {
	if !Locked(b) {
		return b, nil
	}
	if password == "" {
		return nil, ErrLocked
	}
	raw, err := hex.DecodeString(string(bytes.TrimSpace(b[len(prefix):])))
	if err != nil || len(raw) < 16+8+12+16 {
		return nil, ErrPassword
	}
	salt, time, mem := raw[:16], binary.BigEndian.Uint32(raw[16:20]), binary.BigEndian.Uint32(raw[20:24])
	if time == 0 || time > 64 || mem < 1024 || mem > 4<<20 {
		return nil, ErrPassword
	}
	block, err := aes.NewCipher(derive(password, salt, time, mem))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := raw[24 : 24+gcm.NonceSize()]
	plain, err := gcm.Open(nil, nonce, raw[24+gcm.NonceSize():], []byte(prefix))
	if err != nil {
		return nil, ErrPassword
	}
	return plain, nil
}

// Read returns a key file's content, unlocking it with password if needed.
func Read(path, password string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	plain, err := Unseal(b, password)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return plain, nil
}

// Write stores a key file atomically, locked when password is set.
func Write(path string, plain []byte, password string) error {
	out := plain
	if password != "" {
		var err error
		if out, err = Seal(plain, password); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(out); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// State reports which secret key files of dir are plain and which locked.
func State(dir string) (plain, locked []string) {
	for _, name := range SecretFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if Locked(b) {
			locked = append(locked, name)
		} else {
			plain = append(plain, name)
		}
	}
	return plain, locked
}

// Check reports whether password opens every locked file of dir.
func Check(dir, password string) error {
	_, locked := State(dir)
	for _, name := range locked {
		if _, err := Read(filepath.Join(dir, name), password); err != nil {
			return err
		}
	}
	return nil
}

// Rekey locks every secret key file of dir with newPassword (empty: plain),
// opening locked ones with oldPassword. Plain files are locked too, so this
// also moves a node from plain keys to a password.
func Rekey(dir, oldPassword, newPassword string) error {
	if err := Check(dir, oldPassword); err != nil {
		return err
	}
	for _, name := range SecretFiles {
		path := filepath.Join(dir, name)
		plain, err := Read(path, oldPassword)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := Write(path, plain, newPassword); err != nil {
			return err
		}
	}
	return nil
}
