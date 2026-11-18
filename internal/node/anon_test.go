package node

import (
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
)

// Message ids survive a restart, so a node can never spend the same id on
// different content (which would reveal its secret).
func TestMsgIDLedgerSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	n := &Node{dir: dir}
	if err := n.loadRLN(); err != nil {
		t.Fatal(err)
	}
	if err := n.newIdentity(); err != nil {
		t.Fatal(err)
	}
	s1, s2, s3 := cryptox.Hash{1}, cryptox.Hash{2}, cryptox.Hash{3}
	if id, err := n.allocMsgID(7, 2, s1); err != nil || id != 0 {
		t.Fatal(id, err)
	}
	if id, err := n.allocMsgID(7, 2, s2); err != nil || id != 1 {
		t.Fatal(id, err)
	}
	m := &Node{dir: dir}
	if err := m.loadRLN(); err != nil {
		t.Fatal(err)
	}
	if *m.secret != *n.secret {
		t.Fatal("identity not reloaded")
	}
	if id, err := m.allocMsgID(7, 2, s2); err != nil || id != 1 {
		t.Fatal("same content must reuse its id", id, err)
	}
	if _, err := m.allocMsgID(7, 2, s3); err == nil {
		t.Fatal("limit not enforced after restart")
	}
	if id, err := m.allocMsgID(8, 2, s3); err != nil || id != 0 {
		t.Fatal("new epoch starts at 0", id, err)
	}
}
