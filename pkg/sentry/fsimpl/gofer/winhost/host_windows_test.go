//go:build windows

package winhost

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
	"golang.org/x/sys/windows"
)

func newFS(t *testing.T, opts Options) (*FS, string) {
	t.Helper()
	dir := t.TempDir()
	fs, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(fs.Close)
	return fs, dir
}

func mustCreate(t *testing.T, fs *FS, dir ID, name string, spec Spec) Attr {
	t.Helper()
	a, err := fs.Create(dir, name, spec)
	if err != nil {
		t.Fatalf("Create(%q, %#o): %v", name, spec.Mode, err)
	}
	return a
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: got %v, want %v", what, err, want)
	}
}

func TestCreateReadWriteRenameUnlink(t *testing.T) {
	fs, dir := newFS(t, Options{})
	root := fs.Root()
	a := mustCreate(t, fs, root, "f", Spec{Mode: 0o644})
	if a.Mode != SIFREG|0o644 || a.Nlink != 1 || a.Size != 0 {
		t.Errorf("new file attr: %+v", a)
	}
	if n, err := fs.WriteAt(a.ID, []byte("hello world"), 0); err != nil || n != 11 {
		t.Fatalf("WriteAt = %d, %v", n, err)
	}
	if _, err := fs.WriteAt(a.ID, []byte("X"), 100); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if n, err := fs.ReadAt(a.ID, buf, 6); err != nil || n != 5 || string(buf) != "world" {
		t.Errorf("ReadAt = %d, %v, %q", n, err, buf)
	}
	if n, _ := fs.ReadAt(a.ID, buf, 50); n != 5 || !bytes.Equal(buf, make([]byte, 5)) {
		t.Errorf("hole read = %d %q", n, buf)
	}
	if n, err := fs.ReadAt(a.ID, buf, 1000); err != nil || n != 0 {
		t.Errorf("read past EOF = %d, %v", n, err)
	}
	if st, _ := fs.Stat(a.ID); st.Size != 101 {
		t.Errorf("size = %d, want 101", st.Size)
	}
	if err := fs.Truncate(a.ID, 3); err != nil {
		t.Fatal(err)
	}
	if st, _ := fs.Stat(a.ID); st.Size != 3 {
		t.Errorf("size after truncate = %d", st.Size)
	}
	if err := fs.Sync(a.ID); err != nil {
		t.Errorf("Sync: %v", err)
	}

	_, err := fs.Create(root, "f", Spec{Mode: 0o644})
	wantErr(t, "create existing", err, unix.EEXIST)
	_, err = fs.Lookup(root, "missing")
	wantErr(t, "lookup missing", err, unix.ENOENT)

	d := mustCreate(t, fs, root, "d", Spec{Mode: SIFDIR | 0o755})
	if d.Mode != SIFDIR|0o755 || d.Nlink < 2 {
		t.Errorf("dir attr %+v", d)
	}
	if err := fs.Rename(root, "f", d.ID, "g", 0); err != nil {
		t.Fatalf("Rename across dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d", "g")); err != nil {
		t.Errorf("renamed file not on disk: %v", err)
	}
	g, err := fs.Lookup(d.ID, "g")
	if err != nil || g.ID != a.ID {
		t.Errorf("after rename: %+v %v (want id %v)", g, err, a.ID)
	}
	wantErr(t, "rmdir non-empty", fs.Unlink(root, "d", true), unix.ENOTEMPTY)
	wantErr(t, "unlink a directory", fs.Unlink(root, "d", false), unix.EISDIR)
	wantErr(t, "rmdir a file", fs.Unlink(d.ID, "g", true), unix.ENOTDIR)
	if err := fs.Unlink(d.ID, "g", false); err != nil {
		t.Fatal(err)
	}
	if err := fs.Unlink(root, "d", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d")); !os.IsNotExist(err) {
		t.Errorf("dir still there: %v", err)
	}
}

func TestRenameSemantics(t *testing.T) {
	fs, _ := newFS(t, Options{})
	root := fs.Root()
	f1 := mustCreate(t, fs, root, "f1", Spec{Mode: 0o644})
	f2 := mustCreate(t, fs, root, "f2", Spec{Mode: 0o644})
	fs.WriteAt(f1.ID, []byte("one"), 0)
	fs.WriteAt(f2.ID, []byte("two"), 0)
	if err := fs.Rename(root, "f1", root, "f2", 0); err != nil {
		t.Fatalf("replace file: %v", err)
	}
	if a, err := fs.Lookup(root, "f2"); err != nil || a.ID != f1.ID {
		t.Errorf("f2 is not f1 after replace: %+v %v", a, err)
	}
	if _, err := fs.Lookup(root, "f1"); !errors.Is(err, unix.ENOENT) {
		t.Errorf("f1 still exists: %v", err)
	}
	f3 := mustCreate(t, fs, root, "f3", Spec{Mode: 0o644})
	wantErr(t, "noreplace", fs.Rename(root, "f3", root, "f2", RenameNoReplace), unix.EEXIST)
	wantErr(t, "exchange", fs.Rename(root, "f3", root, "f2", RenameExchange), unix.EINVAL)
	_ = f3

	d1 := mustCreate(t, fs, root, "d1", Spec{Mode: SIFDIR | 0o755})
	d2 := mustCreate(t, fs, root, "d2", Spec{Mode: SIFDIR | 0o755})
	if err := fs.Rename(root, "d1", root, "d2", 0); err != nil {
		t.Errorf("replace empty dir: %v", err)
	} else if a, _ := fs.Lookup(root, "d2"); a.ID != d1.ID {
		t.Errorf("d2 is not d1")
	}
	_ = d2
	d3 := mustCreate(t, fs, root, "d3", Spec{Mode: SIFDIR | 0o755})
	mustCreate(t, fs, d3.ID, "x", Spec{Mode: 0o644})
	mustCreate(t, fs, root, "d4", Spec{Mode: SIFDIR | 0o755})
	wantErr(t, "dir over non-empty dir", fs.Rename(root, "d4", root, "d3", 0), unix.ENOTEMPTY)
	wantErr(t, "file over dir", fs.Rename(root, "f2", root, "d3", 0), unix.EISDIR)
	wantErr(t, "dir over file", fs.Rename(root, "d4", root, "f2", 0), unix.ENOTDIR)
	// A directory with files moves as a whole.
	if err := fs.Rename(root, "d3", root, "d5", 0); err != nil {
		t.Errorf("rename dir with content: %v", err)
	}
	if _, err := fs.Lookup(d3.ID, "x"); err != nil {
		t.Errorf("content lost after dir rename: %v", err)
	}
}

func TestHardLinks(t *testing.T) {
	fs, _ := newFS(t, Options{})
	root := fs.Root()
	a := mustCreate(t, fs, root, "a", Spec{Mode: 0o600, UID: 5, GID: 6})
	b, err := fs.Link(root, "b", a.ID)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if b.ID != a.ID || b.Nlink != 2 {
		t.Errorf("link attr %+v, want same id and nlink 2", b)
	}
	if b.Mode != SIFREG|0o600 || b.UID != 5 || b.GID != 6 {
		t.Errorf("link shares metadata: got mode %#o uid %d gid %d", b.Mode, b.UID, b.GID)
	}
	fs.WriteAt(a.ID, []byte("shared"), 0)
	got := make([]byte, 6)
	if n, _ := fs.ReadAt(b.ID, got, 0); n != 6 || string(got) != "shared" {
		t.Errorf("read through link: %q", got)
	}
	if err := fs.Unlink(root, "a", false); err != nil {
		t.Fatal(err)
	}
	if st, err := fs.Stat(b.ID); err != nil || st.Nlink != 1 {
		t.Errorf("after unlink: %+v %v", st, err)
	}
	d := mustCreate(t, fs, root, "d", Spec{Mode: SIFDIR | 0o755})
	_, err = fs.Link(root, "dl", d.ID)
	wantErr(t, "link to directory", err, unix.EPERM)
	_, err = fs.Link(root, "b", b.ID)
	wantErr(t, "link over existing", err, unix.EEXIST)
}

func testSymlinks(t *testing.T, emulate bool) {
	fs, dir := newFS(t, Options{EmulateSymlinks: emulate})
	root := fs.Root()
	f := mustCreate(t, fs, root, "target", Spec{Mode: 0o644})
	for _, target := range []string{"target", "sub/dir/../x", "/usr/bin/env", "a:b", `back\slash`, "dangling"} {
		name := "l-" + fmt.Sprint(len(target))
		a := mustCreate(t, fs, root, name, Spec{Mode: SIFLNK | 0o777, UID: 1, GID: 2, Target: target})
		if a.Mode != SIFLNK|0o777 {
			t.Errorf("%q: mode %#o", target, a.Mode)
		}
		got, err := fs.Readlink(a.ID)
		if err != nil || got != target {
			t.Errorf("Readlink(%q) = %q, %v", target, got, err)
		}
		l, err := fs.Lookup(root, name)
		if err != nil || l.ID != a.ID || l.Mode&SIFMT != SIFLNK {
			t.Errorf("lookup of link to %q: %+v, %v", target, l, err)
		}
		if err := fs.Unlink(root, name, false); err != nil {
			t.Errorf("unlink link to %q: %v", target, err)
		}
	}
	// A relative link is a real NTFS symlink unless emulation is forced.
	a := mustCreate(t, fs, root, "rel", Spec{Mode: SIFLNK | 0o777, Target: "target"})
	fi, err := os.Lstat(filepath.Join(dir, "rel"))
	if err != nil {
		t.Fatal(err)
	}
	native := fi.Mode()&os.ModeSymlink != 0
	t.Logf("emulate=%v: relative symlink is native NTFS symlink: %v", emulate, native)
	if !emulate && !native {
		t.Skip("this user cannot create NTFS symlinks (no developer mode, not elevated): emulated links were used, as designed")
	}
	if native == emulate {
		t.Errorf("native = %v with EmulateSymlinks = %v", native, emulate)
	}
	if native {
		if data, err := os.ReadFile(filepath.Join(dir, "rel")); err != nil || len(data) != 0 {
			t.Errorf("reading through the native link: %q %v", data, err)
		}
	}
	// The guest sees the link, not what it points to, in a listing and stat.
	var types = map[string]uint8{}
	fs.ReadDir(root, func(d Dirent) bool { types[d.Name] = d.Type; return true })
	t.Logf("listing types emulate=%v: %v", emulate, types)
	if native && types["rel"] != 10 {
		t.Errorf("native link listed with type %d", types["rel"])
	}
	_ = a
	_ = f
}

func TestSymlinksNative(t *testing.T)   { testSymlinks(t, false) }
func TestSymlinksEmulated(t *testing.T) { testSymlinks(t, true) }

func TestChmodChownRoundTrip(t *testing.T) {
	fs, dir := newFS(t, Options{UID: 1000, GID: 1000})
	root := fs.Root()
	a := mustCreate(t, fs, root, "f", Spec{Mode: 0o644, UID: 1000, GID: 1000})
	d := mustCreate(t, fs, root, "d", Spec{Mode: SIFDIR | 0o755, UID: 1000, GID: 1000})
	if a.UID != 1000 || a.Mode != SIFREG|0o644 {
		t.Fatalf("defaults: %+v", a)
	}
	before, _ := os.Stat(filepath.Join(dir, "f"))
	if err := fs.SetAttr(a.ID, SetAttr{HasMode: true, Mode: 0o4755, HasUID: true, UID: 0, HasGID: true, GID: 42}); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetAttr(d.ID, SetAttr{HasMode: true, Mode: 0o1777, HasUID: true, UID: 7}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(dir, "f"))
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("chmod/chown changed mtime: %v -> %v", before.ModTime(), after.ModTime())
	}
	check := func(fs *FS, when string) {
		st, err := fs.Stat(a.ID)
		if err != nil || st.Mode != SIFREG|0o4755 || st.UID != 0 || st.GID != 42 {
			t.Errorf("%s: file %+v %v", when, st, err)
		}
		st, err = fs.Stat(d.ID)
		if err != nil || st.Mode != SIFDIR|0o1777 || st.UID != 7 || st.GID != 1000 {
			t.Errorf("%s: dir %+v %v", when, st, err)
		}
	}
	check(fs, "same mount")
	fs2, err := Open(dir, Options{UID: 1000, GID: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer fs2.Close()
	check(fs2, "reopened mount")

	// A file made on Windows gets the defaults.
	os.WriteFile(filepath.Join(dir, "win.txt"), []byte("x"), 0o666)
	w, err := fs.Lookup(root, "win.txt")
	if err != nil || w.Mode != SIFREG|0o644 || w.UID != 1000 || w.GID != 1000 {
		t.Errorf("windows file: %+v %v", w, err)
	}
	os.Mkdir(filepath.Join(dir, "windir"), 0o777)
	if w, err := fs.Lookup(root, "windir"); err != nil || w.Mode != SIFDIR|0o755 {
		t.Errorf("windows dir: %+v %v", w, err)
	}
	// Truncating and writing keep the metadata stream.
	fs.WriteAt(a.ID, []byte("data"), 0)
	fs.Truncate(a.ID, 0)
	if st, _ := fs.Stat(a.ID); st.Mode != SIFREG|0o4755 {
		t.Errorf("mode lost after write+truncate: %#o", st.Mode)
	}
}

func TestMknodRoundTrip(t *testing.T) {
	fs, dir := newFS(t, Options{})
	root := fs.Root()
	specs := map[string]Spec{
		"fifo": {Mode: SIFIFO | 0o600},
		"sock": {Mode: SIFSOCK | 0o755},
		"null": {Mode: SIFCHR | 0o666, RdevMajor: 1, RdevMinor: 3},
		"sda1": {Mode: SIFBLK | 0o660, RdevMajor: 8, RdevMinor: 1, UID: 0, GID: 6},
	}
	for name, s := range specs {
		a := mustCreate(t, fs, root, name, s)
		if a.Mode != s.Mode || a.RdevMajor != s.RdevMajor || a.RdevMinor != s.RdevMinor {
			t.Errorf("%s: created %+v", name, a)
		}
	}
	fs2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer fs2.Close()
	for name, s := range specs {
		a, err := fs2.Lookup(root, name)
		if err != nil || a.Mode != s.Mode || a.RdevMajor != s.RdevMajor || a.RdevMinor != s.RdevMinor || a.GID != s.GID {
			t.Errorf("%s after reopen: %+v %v", name, a, err)
		}
		if a.Size != 0 {
			t.Errorf("%s: size %d", name, a.Size)
		}
	}
	types := map[string]uint8{}
	fs.ReadDir(root, func(d Dirent) bool { types[d.Name] = d.Type; return true })
	for name := range specs {
		if types[name] != 0 {
			t.Errorf("%s listed as type %d, want DT_UNKNOWN", name, types[name])
		}
	}
}

func TestXattrs(t *testing.T) {
	fs, dir := newFS(t, Options{})
	root := fs.Root()
	f := mustCreate(t, fs, root, "f", Spec{Mode: 0o644})
	d := mustCreate(t, fs, root, "d", Spec{Mode: SIFDIR | 0o755})
	wh := mustCreate(t, fs, root, "gone", Spec{Mode: SIFCHR, RdevMajor: 0, RdevMinor: 0})

	if _, err := fs.GetXattr(f.ID, "user.a"); !errors.Is(err, unix.ENODATA) {
		t.Errorf("get missing: %v", err)
	}
	if names, err := fs.ListXattr(f.ID); err != nil || len(names) != 0 {
		t.Errorf("list empty: %v %v", names, err)
	}
	if err := fs.SetXattr(f.ID, "user.a", []byte("1"), 0); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "create existing", fs.SetXattr(f.ID, "user.a", []byte("2"), xattrCreate), unix.EEXIST)
	wantErr(t, "replace missing", fs.SetXattr(f.ID, "user.b", []byte("2"), xattrReplace), unix.ENODATA)
	if err := fs.SetXattr(f.ID, "security.capability", []byte{1, 0, 0, 2, 0xff, 0xff, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 0); err != nil {
		t.Fatal(err)
	}
	// Overlay: the whiteout is a 0/0 char device, opaque dirs carry an xattr.
	if err := fs.SetXattr(d.ID, "trusted.overlay.opaque", []byte("y"), 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetXattr(wh.ID, "trusted.overlay.whiteout", []byte{}, 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetXattr(f.ID, "trusted.overlay.redirect", []byte("/old/name"), 0); err != nil {
		t.Fatal(err)
	}
	fs2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer fs2.Close()
	if v, err := fs2.GetXattr(f.ID, "user.a"); err != nil || string(v) != "1" {
		t.Errorf("user.a = %q %v", v, err)
	}
	if v, err := fs2.GetXattr(f.ID, "security.capability"); err != nil || len(v) != 20 || v[3] != 2 {
		t.Errorf("security.capability = %v %v", v, err)
	}
	if v, err := fs2.GetXattr(d.ID, "trusted.overlay.opaque"); err != nil || string(v) != "y" {
		t.Errorf("opaque = %q %v", v, err)
	}
	if v, err := fs2.GetXattr(wh.ID, "trusted.overlay.whiteout"); err != nil || len(v) != 0 {
		t.Errorf("whiteout = %q %v", v, err)
	}
	if st, _ := fs2.Stat(wh.ID); st.Mode != SIFCHR {
		t.Errorf("whiteout mode %#o", st.Mode)
	}
	names, _ := fs2.ListXattr(f.ID)
	want := []string{"security.capability", "trusted.overlay.redirect", "user.a"}
	if !sort.StringsAreSorted(names) || fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("list = %v, want %v", names, want)
	}
	if err := fs2.RemoveXattr(f.ID, "user.a"); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "remove missing", fs2.RemoveXattr(f.ID, "user.a"), unix.ENODATA)
	// Xattrs and chmod share the stream and must not clobber each other.
	fs2.SetAttr(f.ID, SetAttr{HasMode: true, Mode: 0o600})
	if v, err := fs2.GetXattr(f.ID, "trusted.overlay.redirect"); err != nil || string(v) != "/old/name" {
		t.Errorf("xattr lost after chmod: %q %v", v, err)
	}
	if st, _ := fs2.Stat(f.ID); st.Mode != SIFREG|0o600 {
		t.Errorf("mode lost after setxattr: %#o", st.Mode)
	}
}

func TestReadDirStableInodes(t *testing.T) {
	fs, dir := newFS(t, Options{})
	root := fs.Root()
	want := map[string]ID{}
	for _, n := range []string{"a", "b", "c d", "e:f", "ü"} {
		want[n] = mustCreate(t, fs, root, n, Spec{Mode: 0o644}).ID
	}
	sub := mustCreate(t, fs, root, "sub", Spec{Mode: SIFDIR | 0o755})
	want["sub"] = sub.ID
	list := func(fs *FS) map[string]Dirent {
		got := map[string]Dirent{}
		err := fs.ReadDir(root, func(d Dirent) bool { got[d.Name] = d; return true })
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := list(fs)
	if len(got) != len(want) {
		t.Errorf("listed %d entries, want %d: %v", len(got), len(want), got)
	}
	for n, id := range want {
		d, ok := got[n]
		if !ok {
			t.Errorf("missing %q", n)
			continue
		}
		if d.ID != id {
			t.Errorf("%q: listing id %v, stat id %v", n, d.ID, id)
		}
		if d.ID.Ino() != id.Ino() || id.Ino() == 0 {
			t.Errorf("%q: ino %d vs %d", n, d.ID.Ino(), id.Ino())
		}
	}
	if got["sub"].Type != 4 || got["a"].Type != 8 {
		t.Errorf("types: sub=%d a=%d", got["sub"].Type, got["a"].Type)
	}
	// Same numbers after a remount and after a rename.
	fs2, _ := Open(dir, Options{})
	defer fs2.Close()
	for n, d := range list(fs2) {
		if d.ID.Ino() != want[n].Ino() {
			t.Errorf("%q: ino changed across mounts", n)
		}
	}
	ino := want["a"].Ino()
	fs.Rename(root, "a", sub.ID, "moved", 0)
	var moved Dirent
	fs.ReadDir(sub.ID, func(d Dirent) bool { moved = d; return true })
	if moved.Name != "moved" || moved.ID.Ino() != ino {
		t.Errorf("after rename: %+v, want ino %d", moved, ino)
	}
	// Early stop.
	n := 0
	fs.ReadDir(root, func(Dirent) bool { n++; return false })
	if n != 1 {
		t.Errorf("callback returned false but ran %d times", n)
	}
}

func TestLargeDirectory(t *testing.T) {
	fs, dir := newFS(t, Options{})
	root := fs.Root()
	const N = 20000
	for i := 0; i < N; i++ {
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("file-with-a-longer-name-%06d", i)))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	seen := map[string]bool{}
	inos := map[uint64]bool{}
	err := fs.ReadDir(root, func(d Dirent) bool {
		if seen[d.Name] {
			t.Errorf("duplicate %q", d.Name)
		}
		seen[d.Name] = true
		inos[d.ID.Ino()] = true
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != N || len(inos) != N {
		t.Errorf("listed %d names and %d inodes, want %d", len(seen), len(inos), N)
	}
}

func TestNamesWithReservedCharacters(t *testing.T) {
	fs, dir := newFS(t, Options{})
	root := fs.Root()
	names := []string{"a:b", "x:gvisor.meta", "q?", `s\t`, "w*", "t<>|\"", "name:"}
	for _, n := range names {
		a := mustCreate(t, fs, root, n, Spec{Mode: 0o600, UID: 3})
		fs.WriteAt(a.ID, []byte("data-"+n), 0)
		got, err := fs.Lookup(root, n)
		if err != nil || got.ID != a.ID || got.UID != 3 {
			t.Errorf("%q: lookup %+v %v", n, got, err)
		}
	}
	listed := map[string]bool{}
	fs.ReadDir(root, func(d Dirent) bool { listed[d.Name] = true; return true })
	for _, n := range names {
		if !listed[n] {
			t.Errorf("%q missing from listing %v", n, listed)
		}
	}
	// "x:gvisor.meta" must be a file of its own, not the metadata of "x".
	if _, err := fs.Lookup(root, "x"); !errors.Is(err, unix.ENOENT) {
		t.Errorf("lookup x: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != len(names) {
		t.Errorf("%d files on disk, want %d", len(ents), len(names))
	}
	for _, n := range names {
		if err := fs.Unlink(root, n, false); err != nil {
			t.Errorf("unlink %q: %v", n, err)
		}
	}
}

// A file the backend holds can be renamed and deleted from Windows, and the
// guest keeps reaching it.
func TestWindowsRenamesAndDeletesOpenFiles(t *testing.T) {
	fs, dir := newFS(t, Options{})
	root := fs.Root()
	a := mustCreate(t, fs, root, "open.txt", Spec{Mode: 0o644})
	fs.WriteAt(a.ID, []byte("content"), 0)
	if err := fs.Pin(a.ID); err != nil { // the strongest case: a handle kept open
		t.Fatal(err)
	}
	p := func(n string) string { return filepath.Join(dir, n) }
	if err := os.Rename(p("open.txt"), p("renamed.txt")); err != nil {
		t.Fatalf("Windows rename of a file the backend holds: %v", err)
	}
	buf := make([]byte, 7)
	if n, err := fs.ReadAt(a.ID, buf, 0); err != nil || string(buf[:n]) != "content" {
		t.Errorf("read after rename: %q %v", buf[:n], err)
	}
	if got, err := fs.Lookup(root, "renamed.txt"); err != nil || got.ID != a.ID {
		t.Errorf("lookup of new name: %+v %v", got, err)
	}
	if err := os.Remove(p("renamed.txt")); err != nil {
		t.Fatalf("Windows delete of a file the backend holds: %v", err)
	}
	if _, err := os.Stat(p("renamed.txt")); !os.IsNotExist(err) {
		t.Errorf("name still present after delete: %v", err)
	}
	// Linux keeps the data of an unlinked open file.
	if n, err := fs.ReadAt(a.ID, buf, 0); err != nil || string(buf[:n]) != "content" {
		t.Errorf("read of deleted but pinned file: %q %v", buf[:n], err)
	}
	if _, err := fs.WriteAt(a.ID, []byte("more"), 7); err != nil {
		t.Errorf("write to deleted pinned file: %v", err)
	}
	// The name is reusable at once.
	if err := os.WriteFile(p("renamed.txt"), []byte("new"), 0o666); err != nil {
		t.Errorf("recreating the deleted name: %v", err)
	}
	fs.Unpin(a.ID)
	if _, err := fs.Stat(a.ID); err == nil {
		t.Errorf("deleted file reachable after unpin")
	}
}

// The backend itself unlinks a file that the guest still has open.
func TestGuestUnlinkOfPinnedFile(t *testing.T) {
	fs, _ := newFS(t, Options{})
	root := fs.Root()
	a := mustCreate(t, fs, root, "tmp", Spec{Mode: 0o644})
	fs.WriteAt(a.ID, []byte("abc"), 0)
	if err := fs.Pin(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := fs.Unlink(root, "tmp", false); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Lookup(root, "tmp"); !errors.Is(err, unix.ENOENT) {
		t.Errorf("lookup after unlink: %v", err)
	}
	st, err := fs.Stat(a.ID)
	if err != nil || st.Size != 3 || st.Nlink != 0 {
		t.Errorf("stat of unlinked pinned file: %+v %v", st, err)
	}
	buf := make([]byte, 3)
	if n, _ := fs.ReadAt(a.ID, buf, 0); n != 3 || string(buf) != "abc" {
		t.Errorf("read: %q", buf)
	}
	// The name can be created again while the old file lives on.
	b := mustCreate(t, fs, root, "tmp", Spec{Mode: 0o644})
	if b.ID == a.ID {
		t.Errorf("new file has the old id")
	}
	fs.Unpin(a.ID)
}

// Renaming a directory while a descendant file is held open (the question is
// whether Windows allows it).
func TestRenameDirectoryWithHeldDescendant(t *testing.T) {
	fs, _ := newFS(t, Options{})
	root := fs.Root()
	d := mustCreate(t, fs, root, "d", Spec{Mode: SIFDIR | 0o755})
	f := mustCreate(t, fs, d.ID, "f", Spec{Mode: 0o644})
	if err := fs.Pin(f.ID); err != nil {
		t.Fatal(err)
	}
	defer fs.Unpin(f.ID)
	err := fs.Rename(root, "d", root, "d2", 0)
	t.Logf("rename of a directory with a held descendant: %v", err)
	if err == nil {
		if _, err := fs.Lookup(d.ID, "f"); err != nil {
			t.Errorf("descendant unreachable: %v", err)
		}
	}
	err = fs.Unlink(root, "d2", true)
	t.Logf("rmdir of a non-empty directory with a held descendant: %v", err)
}

func TestCaseSensitivity(t *testing.T) {
	fs, dir := newFS(t, Options{})
	root := fs.Root()
	d := mustCreate(t, fs, root, "ci", Spec{Mode: SIFDIR | 0o755})
	cs, err := fs.IsCaseSensitive(d.ID)
	t.Logf("a new directory is case sensitive: %v (%v)", cs, err)
	if err == nil && cs {
		t.Errorf("directory is case sensitive by default")
	}
	mustCreate(t, fs, d.ID, "File", Spec{Mode: 0o644})
	_, err = fs.Create(d.ID, "file", Spec{Mode: 0o644})
	t.Logf("creating file next to File in a default directory: %v", err)
	if got, err := fs.Lookup(d.ID, "FILE"); err == nil {
		t.Logf("lookup FILE finds %q-equivalent: id match=%v", "File", got.ID != ID{})
	} else {
		t.Logf("lookup FILE: %v", err)
	}

	// Per-directory case sensitivity, on an empty directory.
	s := mustCreate(t, fs, root, "cs", Spec{Mode: SIFDIR | 0o755})
	if err := fs.SetCaseSensitive(s.ID, true); err != nil {
		t.Logf("SetCaseSensitive failed: %v", err)
		t.Skip("per-directory case sensitivity is not available here")
	}
	if cs, _ := fs.IsCaseSensitive(s.ID); !cs {
		t.Fatalf("flag not set")
	}
	x := mustCreate(t, fs, s.ID, "name", Spec{Mode: 0o644})
	y := mustCreate(t, fs, s.ID, "NAME", Spec{Mode: 0o644})
	if x.ID == y.ID {
		t.Errorf("name and NAME are the same file in a case-sensitive directory")
	}
	if got, err := fs.Lookup(s.ID, "Name"); err == nil {
		t.Errorf("lookup of Name found %v in a case-sensitive directory", got.ID)
	}
	if got, err := fs.Lookup(s.ID, "NAME"); err != nil || got.ID != y.ID {
		t.Errorf("lookup NAME: %+v %v", got, err)
	}
	// Directories made below it by the backend inherit or are set.
	sub := mustCreate(t, fs, s.ID, "sub", Spec{Mode: SIFDIR | 0o755})
	cs, _ = fs.IsCaseSensitive(sub.ID)
	t.Logf("subdirectory of a case-sensitive directory is case sensitive: %v", cs)
	_ = dir

	// The CaseSensitive option marks directories the backend creates.
	fs2, err := Open(filepath.Join(dir, "cs", "sub"), Options{CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer fs2.Close()
	n := mustCreate(t, fs2, fs2.Root(), "child", Spec{Mode: SIFDIR | 0o755})
	if cs, _ := fs2.IsCaseSensitive(n.ID); !cs {
		t.Errorf("CaseSensitive option: created directory is not case sensitive")
	}
}

func TestStatfs(t *testing.T) {
	fs, _ := newFS(t, Options{})
	st, err := fs.Statfs()
	if err != nil || st.Blocks == 0 || st.BlocksFree == 0 || st.BlockSize == 0 {
		t.Errorf("statfs: %+v %v", st, err)
	}
}

func TestTimes(t *testing.T) {
	fs, _ := newFS(t, Options{})
	a := mustCreate(t, fs, fs.Root(), "f", Spec{Mode: 0o644})
	at, mt := int64(1_000_000_000_123_456_700), int64(1_500_000_000_987_654_300)
	if err := fs.SetAttr(a.ID, SetAttr{HasAtime: true, Atime: at, HasMtime: true, Mtime: mt}); err != nil {
		t.Fatal(err)
	}
	st, _ := fs.Stat(a.ID)
	if st.Atime != at || st.Mtime != mt {
		t.Errorf("times = %d %d, want %d %d", st.Atime, st.Mtime, at, mt)
	}
	if st.Btime == 0 || st.Ctime == 0 {
		t.Errorf("btime/ctime missing: %+v", st)
	}
}

func TestMetadataCodec(t *testing.T) {
	m := &Metadata{Mode: SIFLNK | 0o777, UID: 1, GID: 2, RdevMajor: 3, RdevMinor: 4, Target: "../x",
		Xattrs: map[string][]byte{"user.b": []byte("2"), "user.a": nil}}
	got, err := DecodeMetadata(m.Encode())
	if err != nil || got.Mode != m.Mode || got.Target != "../x" || len(got.Xattrs) != 2 || string(got.Xattrs["user.b"]) != "2" {
		t.Errorf("round trip: %+v %v", got, err)
	}
	if !bytes.Equal(m.Encode(), m.Encode()) {
		t.Errorf("encoding is not deterministic")
	}
	for _, bad := range [][]byte{nil, []byte("GVM"), []byte("XXXX"), m.Encode()[:30], append(m.Encode()[:4:4], 1, 2)} {
		if _, err := DecodeMetadata(bad); err == nil {
			t.Errorf("decoded %d bad bytes", len(bad))
		}
	}
	if UnescapeName(EscapeName("a:b\\c")) != "a:b\\c" {
		t.Errorf("name escaping does not round trip")
	}
}

// Diagnostic: which way of opening a directory can be enumerated.
func TestProbeEnumeration(t *testing.T) {
	fs, dir := newFS(t, Options{})
	mustCreate(t, fs, fs.Root(), "x", Spec{Mode: 0o644})
	try := func(what string, h windows.Handle) {
		buf := make([]byte, 4096)
		for _, class := range []uint32{20, 11, 15, 2} {
			err := windows.GetFileInformationByHandleEx(h, class, &buf[0], uint32(len(buf)))
			t.Logf("%s class %d: %v", what, class, err)
		}
	}
	p, _ := windows.UTF16PtrFromString(dir)
	h, err := windows.CreateFile(p, 1|0x80|synchronize, shareAll, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	try("CreateFile", h)
	windows.CloseHandle(h)
	for _, acc := range []uint32{1, 1 | 0x80, 1 | 0x20 | 0x80} {
		h, err = ntOpenByID(fs.root, fs.Root(), acc, shareAll, 0)
		t.Logf("by id access %#x: %v", acc, err)
		if err == nil {
			try(fmt.Sprintf("byid %#x", acc), h)
			windows.CloseHandle(h)
		}
	}
	h, err = ntOpenByID(fs.root, fs.Root(), 1|0x80, shareAll, windows.FILE_DIRECTORY_FILE)
	t.Logf("by id with FILE_DIRECTORY_FILE: %v", err)
	if err == nil {
		try("byid dirfile", h)
		windows.CloseHandle(h)
	}
	h, err = ntOpen(fs.root, "", 1|0x80, shareAll, windows.FILE_OPEN, 0, 0)
	t.Logf("relative empty open: %v", err)
}

func TestRenameOverHeldFile(t *testing.T) {
	fs, _ := newFS(t, Options{})
	root := fs.Root()
	src := mustCreate(t, fs, root, "src", Spec{Mode: 0o644})
	dst := mustCreate(t, fs, root, "dst", Spec{Mode: 0o644})
	fs.WriteAt(dst.ID, []byte("old"), 0)
	if err := fs.Pin(dst.ID); err != nil {
		t.Fatal(err)
	}
	defer fs.Unpin(dst.ID)
	if err := fs.Rename(root, "src", root, "dst", 0); err != nil {
		t.Fatalf("rename over a held file: %v", err)
	}
	if a, err := fs.Lookup(root, "dst"); err != nil || a.ID != src.ID {
		t.Errorf("dst is %+v %v", a, err)
	}
	buf := make([]byte, 3)
	if n, _ := fs.ReadAt(dst.ID, buf, 0); string(buf[:n]) != "old" {
		t.Errorf("the replaced file lost its data: %q", buf[:n])
	}
}

// Pins taken, switched to a handle by name and released from many goroutines at
// once (the opens run outside pinMu) end with no pin left and the file reachable.
func TestPinConcurrent(t *testing.T) {
	fs, _ := newFS(t, Options{})
	root := fs.Root()
	a := mustCreate(t, fs, root, "pinned", Spec{Mode: 0o644})
	fs.WriteAt(a.ID, []byte("abc"), 0)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := fs.Pin(a.ID); err != nil {
					t.Errorf("pin: %v", err)
					return
				}
				fs.pinByName(a.ID)
				fs.Unpin(a.ID)
			}
		}()
	}
	wg.Wait()
	fs.pinMu.Lock()
	n := len(fs.pins)
	fs.pinMu.Unlock()
	if n != 0 {
		t.Fatalf("%d pins left", n)
	}
	buf := make([]byte, 3)
	if n, _ := fs.ReadAt(a.ID, buf, 0); n != 3 || string(buf) != "abc" {
		t.Errorf("read after the pins: %q", buf[:n])
	}
}
