package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSocketDirectoryReplacement(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			path := fakeChrony(t)
			c, err := prepareChronySocket(path)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			moved := c.dir + ".moved"
			if err := os.Rename(c.dir, moved); err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(moved)
			if replacement == "directory" {
				err = os.Mkdir(c.dir, 0o700)
			} else {
				// A link to the daemon's directory must not redirect bind or chmod.
				err = os.Symlink(filepath.Dir(path), c.dir)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(c.dir)
			if err := c.connectUnix(path); err == nil {
				t.Fatal("bound a socket through the replacement directory")
			}
			c.Close()
			if _, err := os.Lstat(c.dir); err != nil {
				t.Errorf("removed the replacement: %v", err)
			}
			entries, err := os.ReadDir(moved)
			if err != nil || len(entries) != 0 {
				t.Errorf("did not clean up the pinned original: %v, %v", entries, err)
			}
		})
	}
}

func TestSocketCleanupAfterParentRename(t *testing.T) {
	parent, err := os.MkdirTemp("", "ct-parent-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(parent)
	c, err := prepareChronySocket(filepath.Join(parent, "chronyd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	moved := parent + ".moved"
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(moved)
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := os.Stat(c.dir); err != nil {
		t.Errorf("modified the replacement parent: %v", err)
	}
	entries, err := os.ReadDir(moved)
	if err != nil || len(entries) != 0 {
		t.Errorf("did not clean up the original parent: %v, %v", entries, err)
	}
}

func TestSocketCleanupAfterBindAndRename(t *testing.T) {
	c := connectTest(t, fakeChrony(t))
	for path, mode := range map[string]os.FileMode{
		c.dir:                                    0o711,
		filepath.Join(c.dir, c.replyDir):         0o711,
		filepath.Join(c.dir, c.replyDir, "sock"): 0o666,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("unexpected reply permissions for %s: %v, %v", path, info, err)
		}
	}
	moved := c.dir + ".moved"
	if err := os.Rename(c.dir, moved); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(moved)
	if err := os.Mkdir(c.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(c.dir)
	sentinel := filepath.Join(c.dir, "unrelated")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Close()
	entries, err := os.ReadDir(moved)
	if err != nil || len(entries) != 0 {
		t.Errorf("reply socket remains in renamed directory: %v, %v", entries, err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Errorf("modified unrelated file: %q, %v", data, err)
	}
}
