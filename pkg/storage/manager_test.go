package storage

import (
	"os"
	"path/filepath"
	"testing"

	"tuya-ipc-terminal/pkg/tuya"
)

func TestSaveUserIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewStorageManagerAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.SaveUser("eu-central", "user@example.com", &tuya.SessionData{ServerHost: "example.invalid"}); err != nil {
		t.Fatal(err)
	}
	if err := sm.SaveUser("eu-central", "user@example.com", &tuya.SessionData{ServerHost: "example2.invalid"}); err != nil {
		t.Fatal(err)
	}

	user, err := sm.GetUser("eu-central", "user@example.com")
	if err != nil || user == nil {
		t.Fatalf("GetUser: %v %v", user, err)
	}
	if user.SessionData.ServerHost != "example2.invalid" {
		t.Fatalf("got %q, want latest session", user.SessionData.ServerHost)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly one file (no temp leftovers), got %d", len(entries))
	}
	info, err := os.Stat(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("session file mode %v, want 0600", info.Mode().Perm())
	}
}
