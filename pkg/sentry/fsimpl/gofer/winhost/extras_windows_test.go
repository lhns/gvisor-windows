//go:build windows

package winhost

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// An editor saves by writing a temporary file and renaming it over the
// original: the file ID changes and the stream is gone.
func TestSafeSaveKeepsMetadata(t *testing.T) {
	fs, dir := newFS(t, Options{})
	root := fs.Root()
	a := mustCreate(t, fs, root, "doc.txt", Spec{Mode: 0o600, UID: 5, GID: 6})
	if err := fs.SetXattr(a.ID, "user.k", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Lookup(root, "doc.txt"); err != nil { // the guest looked at it
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "doc.txt.tmp")
	if err := os.WriteFile(tmp, []byte("new content"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "doc.txt")); err != nil {
		t.Fatal(err)
	}
	b, err := fs.Lookup(root, "doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	if b.ID == a.ID {
		t.Fatalf("the file kept its ID, so this was not a replacement")
	}
	if b.Mode != SIFREG|0o600 || b.UID != 5 || b.GID != 6 || b.Size != 11 {
		t.Errorf("after replacement: mode %#o uid %d gid %d size %d", b.Mode, b.UID, b.GID, b.Size)
	}
	if v, err := fs.GetXattr(b.ID, "user.k"); err != nil || string(v) != "v" {
		t.Errorf("xattr after replacement: %q %v", v, err)
	}
	// A second save keeps working.
	os.WriteFile(tmp, []byte("again"), 0o666)
	os.Rename(tmp, filepath.Join(dir, "doc.txt"))
	if c, err := fs.Lookup(root, "doc.txt"); err != nil || c.Mode != SIFREG|0o600 || c.UID != 5 {
		t.Errorf("second replacement: %+v %v", c, err)
	}
	// A name the guest removed does not hand its metadata to a new file.
	if err := fs.Unlink(root, "doc.txt", false); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "doc.txt"), []byte("x"), 0o666)
	if d, err := fs.Lookup(root, "doc.txt"); err != nil || d.Mode != SIFREG|0o644 || d.UID != 0 {
		t.Errorf("new file after guest unlink: %+v %v", d, err)
	}
}

type changes struct {
	mu  sync.Mutex
	got []Change
}

func (c *changes) add(ch Change) { c.mu.Lock(); c.got = append(c.got, ch); c.mu.Unlock() }

func (c *changes) has(path string, a ChangeAction) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.got {
		if ch.Path == path && ch.Action == a {
			return true
		}
	}
	return false
}

func (c *changes) waitFor(path string, a ChangeAction) (time.Duration, bool) {
	start := time.Now()
	for time.Since(start) < 3*time.Second {
		if c.has(path, a) {
			return time.Since(start), true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return 0, false
}

func TestWatchReportsWindowsChanges(t *testing.T) {
	fs, dir := newFS(t, Options{})
	var c changes
	if err := fs.Watch(c.add); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	p := filepath.Join(dir, "w.txt")
	os.WriteFile(p, []byte("1"), 0o666)
	if d, ok := c.waitFor("w.txt", Added); !ok {
		t.Errorf("no Added event")
	} else {
		t.Logf("Added after %v", d)
	}
	os.WriteFile(p, []byte("22"), 0o666)
	if d, ok := c.waitFor("w.txt", Modified); !ok {
		t.Errorf("no Modified event")
	} else {
		t.Logf("Modified after %v", d)
	}
	os.Mkdir(filepath.Join(dir, "sub"), 0o777)
	os.Rename(p, filepath.Join(dir, "sub", "moved.txt"))
	if _, ok := c.waitFor("w.txt", RenamedFrom); !ok {
		t.Errorf("no RenamedFrom event")
	}
	if _, ok := c.waitFor("sub/moved.txt", RenamedTo); !ok {
		t.Errorf("no RenamedTo event")
	}
	os.Remove(filepath.Join(dir, "sub", "moved.txt"))
	if _, ok := c.waitFor("sub/moved.txt", Removed); !ok {
		t.Errorf("no Removed event")
	}
	// The backend's own changes are not reported.
	root := fs.Root()
	a := mustCreate(t, fs, root, "mine.txt", Spec{Mode: 0o600})
	fs.WriteAt(a.ID, []byte("data"), 0)
	fs.SetAttr(a.ID, SetAttr{HasMode: true, Mode: 0o644})
	time.Sleep(500 * time.Millisecond)
	c.mu.Lock()
	for _, ch := range c.got {
		if ch.Path == "mine.txt" {
			t.Errorf("own change reported: %+v", ch)
		}
	}
	c.mu.Unlock()
}
