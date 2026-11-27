package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestReplySocketStaysBesideChronyd(t *testing.T) {
	path := fakeChrony(t)
	c := connectTest(t, path)
	if filepath.Dir(c.replyPath) != filepath.Dir(path) {
		t.Fatalf("reply socket %s is not beside %s", c.replyPath, path)
	}
	if filepath.Base(c.replyPath) != c.replyName || len(c.replyName) < 16 {
		t.Fatalf("unexpected reply name %q", c.replyName)
	}
	info, err := os.Stat(c.replyPath)
	if err != nil || info.Mode().Perm() != 0o666 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("reply socket mode: %v, %v", info, err)
	}
}

func TestReplyNameReplacement(t *testing.T) {
	for _, replacement := range []string{"file", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			path := fakeChrony(t)
			c, err := prepareChronySocket(path)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			secret := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(secret, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if replacement == "file" {
				err = os.WriteFile(c.replyPath, []byte("occupied"), 0o640)
			} else {
				err = os.Symlink(secret, c.replyPath)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(c.replyPath) })
			if err := c.connectUnix(path); err == nil {
				t.Fatal("bound a socket through the replacement")
			}
			info, err := os.Stat(secret)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("replacement redirected chmod: %v, %v", info, err)
			}
			c.Close()
			if _, err := os.Lstat(c.replyPath); err != nil {
				t.Errorf("removed the replacement: %v", err)
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
	socketPath := filepath.Join(parent, "chronyd.sock")
	server, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socketPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	c := connectTest(t, socketPath)
	moved := parent + ".moved"
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(moved)
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(parent)
	planted := filepath.Join(parent, c.replyName)
	if err := os.WriteFile(planted, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if data, err := os.ReadFile(planted); err != nil || string(data) != "keep" {
		t.Errorf("removed the replacement parent's file: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(moved, c.replyName)); !os.IsNotExist(err) {
		t.Errorf("left the reply socket in the original parent: %v", err)
	}
}

func TestSocketCleanupAfterBindAndRename(t *testing.T) {
	c := connectTest(t, fakeChrony(t))
	moved := c.replyPath + ".moved"
	if err := os.Rename(c.replyPath, moved); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(moved)
	if err := os.WriteFile(c.replyPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(c.replyPath)
	c.Close()
	info, err := os.Stat(c.replyPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cleanup changed the replacement: %v, %v", info, err)
	}
	if data, err := os.ReadFile(c.replyPath); err != nil || string(data) != "keep" {
		t.Errorf("modified unrelated file: %q, %v", data, err)
	}
}
