package internal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSafePath(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"../secret", "/etc/passwd", ".git/config", "a/../../secret"} {
		if _, e := safePath(root, path); e == nil {
			t.Errorf("accepted %s", path)
		}
	}
	if _, e := safePath(root, "src/main.ts"); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(t.TempDir(), filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	if _, e := safePath(root, "link/secret"); e == nil {
		t.Fatal("accepted symlink")
	}
}
