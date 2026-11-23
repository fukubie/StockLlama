package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func allowSync(t *testing.T) *int {
	t.Helper()
	calls := 0
	original := syncFile
	syncFile = func(*os.File) error { calls++; return nil }
	t.Cleanup(func() { syncFile = original })
	return &calls
}

func TestAtomicJSONSyncsBeforeRename(t *testing.T) {
	calls := allowSync(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := atomicJSON(path, map[string]string{"provider": "builtin"}); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("atomicJSON called Sync %d times, want 1", *calls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file was not renamed into place: %v", err)
	}
}

func TestAtomicJSONSkipsRenameWhenSyncFails(t *testing.T) {
	original := syncFile
	syncFile = func(*os.File) error { return errors.New("disk went away") }
	t.Cleanup(func() { syncFile = original })
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := atomicJSON(path, map[string]string{"provider": "builtin"}); err == nil {
		t.Fatal("atomicJSON returned nil after Sync failed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("target file exists after a failed Sync: %v", err)
	}
}
