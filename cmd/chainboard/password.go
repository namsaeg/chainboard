package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/namsaeg/chainboard/internal/keystore"
)

var stdin = bufio.NewReader(os.Stdin)

// ask reads one line from the console; hidden turns echo off while typing.
func ask(prompt string, hidden bool) (string, error) {
	fmt.Print(prompt)
	var restore func()
	if hidden {
		restore = echoOff()
	}
	line, err := stdin.ReadString('\n')
	if restore != nil {
		restore()
		fmt.Println()
	}
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// newPassword asks for a password twice. Empty means none.
func newPassword(prompt string) (string, error) {
	for {
		pw, err := ask(prompt, true)
		if err != nil || pw == "" {
			return "", err
		}
		again, err := ask("한 번 더 입력하세요: ", true)
		if err != nil {
			return "", err
		}
		if again == pw {
			return pw, nil
		}
		fmt.Println("두 번 입력한 비밀번호가 달라요. 다시 해 주세요.")
	}
}

// plainMarker records that the user chose to keep keys unlocked, so the
// node does not ask at every start (-change-password locks them later).
const plainMarker = keystore.PlainMarker

// password works out the password of a node directory: from a file or
// CHAINBOARD_PASSWORD, else by asking on the console. Locked keys need it;
// for plain keys the user is offered to set one once.
func password(dir, file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		pw := strings.TrimRight(string(b), "\r\n")
		return pw, keystore.Check(dir, pw)
	}
	if pw := os.Getenv("CHAINBOARD_PASSWORD"); pw != "" {
		return pw, keystore.Check(dir, pw)
	}
	_, locked := keystore.State(dir)
	if len(locked) > 0 {
		if !interactive() {
			return "", errors.New("keys are locked: give -password-file or set CHAINBOARD_PASSWORD")
		}
		for try := 0; try < 3; try++ {
			pw, err := ask("지갑 비밀번호: ", true)
			if err != nil {
				return "", err
			}
			if err := keystore.Check(dir, pw); err == nil {
				return pw, nil
			}
			fmt.Println("비밀번호가 맞지 않아요.")
		}
		return "", keystore.ErrPassword
	}
	if !interactive() {
		return "", nil
	}
	if _, err := os.Stat(filepath.Join(dir, plainMarker)); err == nil {
		return "", nil
	}
	fmt.Println("지갑 키를 비밀번호로 잠글 수 있어요. 잠그면 키 파일을 가져가도 비밀번호 없이는 쓸 수 없어요.")
	fmt.Println("비밀번호를 잊으면 되찾을 방법이 없으니 꼭 따로 적어 두세요.")
	pw, err := newPassword("새 비밀번호 (잠그지 않으려면 그냥 Enter): ")
	if err != nil {
		return "", err
	}
	if pw == "" {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			_ = os.WriteFile(filepath.Join(dir, plainMarker),
				[]byte("keys are kept unlocked; run with -change-password to lock them\n"), 0o600)
		}
	}
	return pw, nil
}

// changePassword locks, relocks or unlocks the keys of a node directory.
// It refuses while a node runs on that directory: the running node would
// keep using the old password and write new key files with it. (The
// settings page changes the password on a running node.)
func changePassword(dir string) error {
	if nodeRunning(dir) {
		return fmt.Errorf("a node is running on %s: change the password in its settings page, or stop it first", dir)
	}
	if !interactive() {
		return errors.New("-change-password needs a console")
	}
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("no node directory at %s (choose it with -datadir and -network)", dir)
	}
	fmt.Printf("이 폴더의 키를 바꿔요: %s\n", dir)
	old := ""
	if _, locked := keystore.State(dir); len(locked) > 0 {
		var err error
		if old, err = ask("지금 비밀번호: ", true); err != nil {
			return err
		}
		if err := keystore.Check(dir, old); err != nil {
			return err
		}
	}
	pw, err := newPassword("새 비밀번호 (빈 칸이면 잠금을 풀어요): ")
	if err != nil {
		return err
	}
	if err := keystore.Rekey(dir, old, pw); err != nil {
		return err
	}
	if pw != "" {
		_ = os.Remove(filepath.Join(dir, plainMarker))
		fmt.Println("키를 새 비밀번호로 잠갔어요.")
	} else {
		_ = os.WriteFile(filepath.Join(dir, plainMarker), []byte("keys are kept unlocked\n"), 0o600)
		fmt.Println("키 잠금을 풀었어요.")
	}
	return nil
}

// nodeRunning reports whether a node holds the chain database of dir: bbolt
// locks the file for as long as a node has it open.
func nodeRunning(dir string) bool {
	path := filepath.Join(dir, "chain.db")
	if _, err := os.Stat(path); err != nil {
		return false
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 300 * time.Millisecond, ReadOnly: true})
	if err != nil {
		return true
	}
	db.Close()
	return false
}
