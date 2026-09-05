package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllRecreatesStateDirectoryRemovedWhileRunning(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	stateDir := filepath.Join(dataDir, "state")
	if err = os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	states, err := store.All()
	if err != nil {
		t.Fatalf("state directory was not recreated: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("unexpected states: %d", len(states))
	}
	info, err := os.Stat(stateDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("state directory was not restored: %v", err)
	}
}
