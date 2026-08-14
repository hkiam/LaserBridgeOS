package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReplacesFileWithRequestedMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("stale"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("fresh"), 0640); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fresh" {
		t.Fatalf("content = %q, want fresh", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// A pre-existing file must not keep its old, wider permissions.
	if info.Mode().Perm() != 0640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
}

func TestWriteLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "keys"), []byte("key\n"), 0644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "keys" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("directory contains %v, want only keys", names)
	}
}

func TestWriteCreatesMissingParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh", "authorized_keys")
	if err := Write(path, []byte("ssh-ed25519 AAAA\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
