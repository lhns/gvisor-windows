// gvisor-windows: tests of the host-folder backend through the VFS layer.

//go:build windows

package gofer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

type testClock struct{}

func (testClock) Now() ktime.Time                     { return ktime.FromNanoseconds(time.Now().UnixNano()) }
func (testClock) NewTimer(ktime.Listener) ktime.Timer { panic("no timers in tests") }

type testContext struct {
	context.Context
	mf *pgalloc.MemoryFile
}

func (c *testContext) Value(key any) any {
	switch key {
	case pgalloc.CtxMemoryFile:
		return c.mf
	case ktime.CtxRealtimeClock:
		return testClock{}
	case limits.CtxLimits:
		return limits.NewLimitSet()
	}
	return c.Context.Value(key)
}

type env struct {
	t     *testing.T
	ctx   context.Context
	v     *vfs.VirtualFilesystem
	creds *auth.Credentials
	root  vfs.VirtualDentry
	dir   string
}

func newEnv(t *testing.T, hf HostFolderOptions) *env {
	t.Helper()
	mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{DisableIMAWorkAround: true})
	if err != nil {
		t.Fatalf("NewMemoryFile: %v", err)
	}
	ctx := &testContext{Context: context.Background(), mf: mf}
	v := &vfs.VirtualFilesystem{}
	if err := v.Init(ctx); err != nil {
		t.Fatal(err)
	}
	v.MustRegisterFilesystemType("gofer", &FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{})
	creds := auth.NewRootCredentials(auth.NewRootUserNamespace())
	hf.Path = t.TempDir()
	mns, err := v.NewMountNamespace(ctx, creds, "", "gofer", &vfs.MountOptions{
		GetFilesystemOptions: vfs.GetFilesystemOptions{InternalData: hf, InternalMount: true},
	}, nil)
	if err != nil {
		t.Fatalf("NewMountNamespace: %v", err)
	}
	return &env{t: t, ctx: ctx, v: v, creds: creds, root: mns.Root(ctx), dir: hf.Path}
}

func (e *env) pop(path string) *vfs.PathOperation {
	return &vfs.PathOperation{Root: e.root, Start: e.root, Path: fspath.Parse(path)}
}

func (e *env) must(what string, err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("%s: %v", what, err)
	}
}

func (e *env) write(path, data string, mode linux.FileMode) {
	e.t.Helper()
	fd, err := e.v.OpenAt(e.ctx, e.creds, e.pop(path), &vfs.OpenOptions{Flags: linux.O_CREAT | linux.O_WRONLY | linux.O_TRUNC, Mode: mode})
	e.must("create "+path, err)
	defer fd.DecRef(e.ctx)
	_, err = fd.Write(e.ctx, usermem.BytesIOSequence([]byte(data)), vfs.WriteOptions{})
	e.must("write "+path, err)
}

func (e *env) read(path string) (string, error) {
	fd, err := e.v.OpenAt(e.ctx, e.creds, e.pop(path), &vfs.OpenOptions{Flags: linux.O_RDONLY})
	if err != nil {
		return "", err
	}
	defer fd.DecRef(e.ctx)
	buf := make([]byte, 4096)
	n, err := fd.PRead(e.ctx, usermem.BytesIOSequence(buf), 0, vfs.ReadOptions{})
	if err != nil && n == 0 && err.Error() != "EOF" {
		return "", err
	}
	return string(buf[:n]), nil
}

func (e *env) stat(path string) (linux.Statx, error) {
	return e.v.StatAt(e.ctx, e.creds, e.pop(path), &vfs.StatOptions{Mask: linux.STATX_BASIC_STATS | linux.STATX_BTIME})
}

func (e *env) list(path string) []string {
	e.t.Helper()
	fd, err := e.v.OpenAt(e.ctx, e.creds, e.pop(path), &vfs.OpenOptions{Flags: linux.O_RDONLY | linux.O_DIRECTORY})
	e.must("open dir "+path, err)
	defer fd.DecRef(e.ctx)
	var names []string
	e.must("iterdirents", fd.IterDirents(e.ctx, vfs.IterDirentsCallbackFunc(func(d vfs.Dirent) error {
		if d.Name != "." && d.Name != ".." {
			names = append(names, d.Name)
		}
		return nil
	})))
	sort.Strings(names)
	return names
}

func TestVFSBasics(t *testing.T) {
	e := newEnv(t, HostFolderOptions{UID: 7, GID: 8})
	e.write("/f", "hello", 0o640)
	if got, err := e.read("/f"); err != nil || got != "hello" {
		t.Errorf("read = %q, %v", got, err)
	}
	st, err := e.stat("/f")
	e.must("stat", err)
	if st.Mode != linux.S_IFREG|0o640 || st.Size != 5 || st.Nlink != 1 || st.Ino == 0 {
		t.Errorf("stat = mode %#o size %d nlink %d ino %d", st.Mode, st.Size, st.Nlink, st.Ino)
	}
	e.must("mkdir", e.v.MkdirAt(e.ctx, e.creds, e.pop("/d"), &vfs.MkdirOptions{Mode: 0o750}))
	e.must("rename", e.v.RenameAt(e.ctx, e.creds, e.pop("/f"), e.pop("/d/g"), &vfs.RenameOptions{}))
	if _, err := e.stat("/f"); !linuxerr.Equals(linuxerr.ENOENT, err) {
		t.Errorf("old name: %v", err)
	}
	if got, _ := e.read("/d/g"); got != "hello" {
		t.Errorf("after rename read = %q", got)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "d", "g")); err != nil {
		t.Errorf("not on the Windows side: %v", err)
	}
	// Replace an existing file.
	e.write("/d/h", "other", 0o644)
	e.must("rename over", e.v.RenameAt(e.ctx, e.creds, e.pop("/d/g"), e.pop("/d/h"), &vfs.RenameOptions{}))
	if got, _ := e.read("/d/h"); got != "hello" {
		t.Errorf("replaced file = %q", got)
	}
	if got := e.list("/d"); fmt.Sprint(got) != "[h]" {
		t.Errorf("list = %v", got)
	}
	e.must("unlink", e.v.UnlinkAt(e.ctx, e.creds, e.pop("/d/h")))
	e.must("rmdir", e.v.RmdirAt(e.ctx, e.creds, e.pop("/d")))
	if got := e.list("/"); len(got) != 0 {
		t.Errorf("root not empty: %v", got)
	}
}

func TestVFSLinksAndSymlinks(t *testing.T) {
	e := newEnv(t, HostFolderOptions{})
	e.write("/a", "x", 0o644)
	e.must("link", e.v.LinkAt(e.ctx, e.creds, e.pop("/a"), e.pop("/b")))
	sa, _ := e.stat("/a")
	sb, err := e.stat("/b")
	e.must("stat b", err)
	if sa.Ino != sb.Ino || sb.Nlink != 2 {
		t.Errorf("link: ino %d vs %d, nlink %d", sa.Ino, sb.Ino, sb.Nlink)
	}
	for _, target := range []string{"a", "/abs/target", "../up"} {
		name := "/l" + fmt.Sprint(len(target))
		e.must("symlink", e.v.SymlinkAt(e.ctx, e.creds, e.pop(name), target))
		got, err := e.v.ReadlinkAt(e.ctx, e.creds, e.pop(name))
		if err != nil || got != target {
			t.Errorf("readlink(%q) = %q, %v", target, got, err)
		}
		st, err := e.v.StatAt(e.ctx, e.creds, e.pop(name), &vfs.StatOptions{Mask: linux.STATX_BASIC_STATS})
		if err != nil || st.Mode&linux.S_IFMT != linux.S_IFLNK {
			t.Errorf("lstat(%q): mode %#o, %v", name, st.Mode, err)
		}
	}
	// Following the link to a reaches the file.
	p := e.pop("/l1")
	p.FollowFinalSymlink = true
	if st, err := e.v.StatAt(e.ctx, e.creds, p, &vfs.StatOptions{Mask: linux.STATX_BASIC_STATS}); err != nil || st.Mode&linux.S_IFMT != linux.S_IFREG {
		t.Errorf("stat through link: %#o %v", st.Mode, err)
	}
}

func TestVFSMetadataMknodXattr(t *testing.T) {
	e := newEnv(t, HostFolderOptions{})
	e.write("/f", "", 0o644)
	e.must("chmod/chown", e.v.SetStatAt(e.ctx, e.creds, e.pop("/f"), &vfs.SetStatOptions{Stat: linux.Statx{
		Mask: linux.STATX_MODE | linux.STATX_UID | linux.STATX_GID, Mode: 0o4711, UID: 1234, GID: 5678,
	}}))
	st, err := e.stat("/f")
	e.must("stat", err)
	if st.Mode != linux.S_IFREG|0o4711 || st.UID != 1234 || st.GID != 5678 {
		t.Errorf("after chmod/chown: mode %#o uid %d gid %d", st.Mode, st.UID, st.GID)
	}
	e.must("truncate", e.v.SetStatAt(e.ctx, e.creds, e.pop("/f"), &vfs.SetStatOptions{Stat: linux.Statx{Mask: linux.STATX_SIZE, Size: 100}}))
	if st, _ := e.stat("/f"); st.Size != 100 {
		t.Errorf("size = %d", st.Size)
	}

	e.must("mknod fifo", e.v.MknodAt(e.ctx, e.creds, e.pop("/fifo"), &vfs.MknodOptions{Mode: linux.S_IFIFO | 0o600}))
	e.must("mknod chr", e.v.MknodAt(e.ctx, e.creds, e.pop("/null"), &vfs.MknodOptions{Mode: linux.S_IFCHR | 0o666, DevMajor: 1, DevMinor: 3}))
	e.must("mknod whiteout", e.v.MknodAt(e.ctx, e.creds, e.pop("/wh"), &vfs.MknodOptions{Mode: linux.S_IFCHR}))
	if st, err := e.stat("/fifo"); err != nil || st.Mode != linux.S_IFIFO|0o600 {
		t.Errorf("fifo: %#o %v", st.Mode, err)
	}
	if st, err := e.stat("/null"); err != nil || st.Mode != linux.S_IFCHR|0o666 || st.RdevMajor != 1 || st.RdevMinor != 3 {
		t.Errorf("chr: %#o %d:%d %v", st.Mode, st.RdevMajor, st.RdevMinor, err)
	}

	for _, x := range []struct{ path, name, val string }{
		{"/wh", "trusted.overlay.whiteout", ""},
		{"/f", "trusted.overlay.opaque", "y"},
		{"/f", "security.capability", "\x01\x00\x00\x02"},
		{"/f", "user.k", "v"},
	} {
		e.must("setxattr "+x.name, e.v.SetXattrAt(e.ctx, e.creds, e.pop(x.path), &vfs.SetXattrOptions{Name: x.name, Value: x.val}))
		got, err := e.v.GetXattrAt(e.ctx, e.creds, e.pop(x.path), &vfs.GetXattrOptions{Name: x.name, Size: 100})
		if err != nil || got != x.val {
			t.Errorf("getxattr %s = %q, %v", x.name, got, err)
		}
	}
	names, err := e.v.ListXattrAt(e.ctx, e.creds, e.pop("/f"), 1000)
	sort.Strings(names)
	if err != nil || fmt.Sprint(names) != "[security.capability trusted.overlay.opaque user.k]" {
		t.Errorf("listxattr = %v, %v", names, err)
	}
	e.must("removexattr", e.v.RemoveXattrAt(e.ctx, e.creds, e.pop("/f"), "user.k"))
	if _, err := e.v.GetXattrAt(e.ctx, e.creds, e.pop("/f"), &vfs.GetXattrOptions{Name: "user.k", Size: 10}); !linuxerr.Equals(linuxerr.ENODATA, err) {
		t.Errorf("getxattr after remove: %v", err)
	}
}

// An open file stays reachable by the guest after Windows renames it, and
// Windows can rename and delete files the guest has open.
func TestVFSWindowsRenamesOpenFile(t *testing.T) {
	e := newEnv(t, HostFolderOptions{})
	e.write("/open", "payload", 0o644)
	fd, err := e.v.OpenAt(e.ctx, e.creds, e.pop("/open"), &vfs.OpenOptions{Flags: linux.O_RDWR})
	e.must("open", err)
	defer fd.DecRef(e.ctx)
	buf := make([]byte, 16)
	if n, _ := fd.PRead(e.ctx, usermem.BytesIOSequence(buf), 0, vfs.ReadOptions{}); string(buf[:n]) != "payload" {
		t.Fatalf("first read %q", buf[:n])
	}
	if err := os.Rename(filepath.Join(e.dir, "open"), filepath.Join(e.dir, "moved")); err != nil {
		t.Fatalf("Windows rename of an open file: %v", err)
	}
	n, err := fd.PRead(e.ctx, usermem.BytesIOSequence(buf), 0, vfs.ReadOptions{})
	t.Logf("read after Windows rename: %q, %v", buf[:n], err)
	if _, err := fd.PWrite(e.ctx, usermem.BytesIOSequence([]byte("PAY")), 0, vfs.WriteOptions{}); err != nil {
		t.Errorf("write after Windows rename: %v", err)
	}
	if err := os.Remove(filepath.Join(e.dir, "moved")); err != nil {
		t.Fatalf("Windows delete of an open file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "moved")); !os.IsNotExist(err) {
		t.Errorf("file still named after delete: %v", err)
	}
	// A new file from Windows shows up without remounting.
	os.WriteFile(filepath.Join(e.dir, "fresh"), []byte("w"), 0o666)
	if got, err := e.read("/fresh"); err != nil || got != "w" {
		t.Errorf("file made on Windows: %q %v", got, err)
	}
}

func TestVFSLargeDirectory(t *testing.T) {
	e := newEnv(t, HostFolderOptions{})
	const n = 5000
	for i := 0; i < n; i++ {
		f, err := os.Create(filepath.Join(e.dir, fmt.Sprintf("entry-%05d", i)))
		e.must("create", err)
		f.Close()
	}
	names := e.list("/")
	if len(names) != n {
		t.Fatalf("listed %d, want %d", len(names), n)
	}
	st1, _ := e.stat("/entry-00042")
	st2, _ := e.stat("/entry-00042")
	if st1.Ino == 0 || st1.Ino != st2.Ino {
		t.Errorf("unstable inode numbers: %d %d", st1.Ino, st2.Ino)
	}
}
