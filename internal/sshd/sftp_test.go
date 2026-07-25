package sshd

import (
	"path/filepath"
	"strings"
	"testing"
)

// An SFTP client is remote input, and the file area sits a couple of
// directories below the node key. A traversal here would reach it.
func TestSFTPRefusesPathTraversal(t *testing.T) {
	root := t.TempDir()
	fs := &areaFS{root: root}

	for _, attack := range []string{
		"../../../etc/passwd",
		"/../../keys/node.ed25519",
		"uploads/../../../../keys/node.ed25519",
		"....//....//keys",
		"/./../../keys",
	} {
		got, err := fs.resolve(attack)
		if err != nil {
			continue // refused outright, which is fine
		}
		if !strings.HasPrefix(got, root) {
			t.Errorf("traversal %q escaped the root: %s", attack, got)
		}
	}
}

func TestSFTPResolvesNormalPaths(t *testing.T) {
	root := t.TempDir()
	fs := &areaFS{root: root}

	got, err := fs.resolve("/uploads/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "uploads", "file.txt")
	if got != want {
		t.Fatalf("resolved to %s, want %s", got, want)
	}

	// The root itself must resolve.
	if _, err := fs.resolve("/"); err != nil {
		t.Fatalf("root path rejected: %v", err)
	}
}

// Uploading requires the capability; reading does not.
func TestSFTPReadOnlyWithoutCapability(t *testing.T) {
	fs := &areaFS{root: t.TempDir(), readOnly: true}
	if _, err := fs.Filewrite(nil); err == nil {
		t.Fatal("a read-only session was allowed to write")
	}
	if err := fs.Filecmd(nil); err == nil {
		t.Fatal("a read-only session was allowed to modify files")
	}
}
