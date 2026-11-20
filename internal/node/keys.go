package node

import (
	"crypto/subtle"
	"errors"
	"os"
	"path/filepath"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/keystore"
)

// Secret key files (keystore.SecretFiles) are read and written only
// through these, under keyMu, with the password the node holds now. A
// password change re-locks every file under the same lock, so no file is
// ever written with a password that is no longer the node's.

func (n *Node) readKey(name string) ([]byte, error) {
	n.keyMu.Lock()
	defer n.keyMu.Unlock()
	return keystore.Read(n.rlnPath(name), n.password)
}

func (n *Node) writeKey(name string, b []byte) error {
	n.keyMu.Lock()
	defer n.keyMu.Unlock()
	return keystore.Write(n.rlnPath(name), b, n.password)
}

// loadKeyFile reads a key, making it if missing.
func (n *Node) loadKeyFile(name string) (*cryptox.Key, error) {
	n.keyMu.Lock()
	defer n.keyMu.Unlock()
	return loadKey(n.rlnPath(name), n.password)
}

// KeyState says how the secret key files are kept: "locked" (all with a
// password), "partial" (some plain: they are locked at the next start with
// a password, or by setting one here), or "plain".
func (n *Node) KeyState() string {
	n.keyMu.Lock()
	defer n.keyMu.Unlock()
	plain, locked := keystore.State(n.dir)
	switch {
	case len(locked) > 0 && len(plain) == 0:
		return "locked"
	case len(locked) > 0:
		return "partial"
	}
	return "plain"
}

// ErrWrongPassword: the current password given to change it is not the
// node's.
var ErrWrongPassword = errors.New("the current password does not match")

// ChangePassword locks the key files with a new password (empty: unlocks
// them), on the running node. The current password must be given when
// there is one.
func (n *Node) ChangePassword(current, next string) error {
	n.keyMu.Lock()
	defer n.keyMu.Unlock()
	if subtle.ConstantTimeCompare([]byte(current), []byte(n.password)) != 1 {
		return ErrWrongPassword
	}
	if err := keystore.Rekey(n.dir, n.password, next); err != nil {
		return err
	}
	n.password = next
	// Remember an explicit choice to keep the keys plain, so the next start
	// does not offer to lock them again.
	marker := filepath.Join(n.dir, keystore.PlainMarker)
	if next == "" {
		return os.WriteFile(marker, []byte("keys are kept unlocked; set a password in the settings or with -change-password\n"), 0o600)
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
