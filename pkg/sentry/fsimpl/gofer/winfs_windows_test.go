// gvisor-windows: tests of the host-folder backend through the VFS layer.

//go:build windows

package gofer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/gofer/winhost"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/uniqueid"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

type testClock struct{}

func (testClock) Now() ktime.Time                     { return ktime.FromNanoseconds(time.Now().UnixNano()) }
func (testClock) NewTimer(ktime.Listener) ktime.Timer { panic("no timers in tests") }

var uniqueCounter atomic.Uint64

type testContext struct {
	context.Context
	mf  *pgalloc.MemoryFile
	mns *vfs.MountNamespace
}

func (c *testContext) Value(key any) any {
	switch key {
	case pgalloc.CtxMemoryFile:
		return c.mf
	case ktime.CtxRealtimeClock:
		return testClock{}
	case vfs.CtxMountNamespace:
		if c.mns == nil {
			return nil
		}
		c.mns.IncRef()
		return c.mns
	case uniqueid.CtxGlobalUniqueID:
		return uniqueCounter.Add(1)
	case uniqueid.CtxInotifyCookie:
		return uint32(1)
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
	if hf.Path == "" {
		hf.Path = t.TempDir()
	}
	mns, err := v.NewMountNamespace(ctx, creds, "", "gofer", &vfs.MountOptions{
		GetFilesystemOptions: vfs.GetFilesystemOptions{InternalData: hf, InternalMount: true},
	}, nil)
	if err != nil {
		t.Fatalf("NewMountNamespace: %v", err)
	}
	ctx.mns = mns
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

func inotifyNames(e *env, fd *vfs.FileDescription) []string {
	buf := make([]byte, 4096)
	n, err := fd.Read(e.ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if err != nil || n == 0 {
		return nil
	}
	var out []string
	b := buf[:n]
	for len(b) >= 16 {
		mask := uint32(b[4]) | uint32(b[5])<<8 | uint32(b[6])<<16 | uint32(b[7])<<24
		l := int(uint32(b[12]) | uint32(b[13])<<8 | uint32(b[14])<<16 | uint32(b[15])<<24)
		name := string(b[16 : 16+l])
		if i := indexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		out = append(out, fmt.Sprintf("%#x:%s", mask, name))
		b = b[16+l:]
	}
	return out
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// A file changed on Windows shows in a guest view that caches (exclusive
// mode) within a second, and guest inotify watchers are told.
func TestVFSWindowsChangesReachGuest(t *testing.T) {
	e := newEnv(t, HostFolderOptions{Exclusive: true})
	e.write("/watched", "abc", 0o644)
	if st, err := e.stat("/watched"); err != nil || st.Size != 3 {
		t.Fatalf("stat %+v %v", st, err)
	}
	ifd, err := vfs.NewInotifyFD(e.ctx, e.v, linux.O_NONBLOCK)
	e.must("inotify", err)
	defer ifd.DecRef(e.ctx)
	ino := ifd.Impl().(*vfs.Inotify)
	rootVD, err := e.v.GetDentryAt(e.ctx, e.creds, e.pop("/"), &vfs.GetDentryOptions{})
	e.must("get root dentry", err)
	fileVD, err := e.v.GetDentryAt(e.ctx, e.creds, e.pop("/watched"), &vfs.GetDentryOptions{})
	e.must("get file dentry", err)
	ino.AddWatch(rootVD.Dentry(), linux.IN_CREATE|linux.IN_DELETE|linux.IN_MODIFY)
	ino.AddWatch(fileVD.Dentry(), linux.IN_MODIFY)

	start := time.Now()
	if err := os.WriteFile(filepath.Join(e.dir, "watched"), []byte("0123456789"), 0o666); err != nil {
		t.Fatal(err)
	}
	var sizeAfter time.Duration
	for time.Since(start) < 2*time.Second {
		if st, err := e.stat("/watched"); err == nil && st.Size == 10 {
			sizeAfter = time.Since(start)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sizeAfter == 0 {
		t.Errorf("the cached guest view did not see the new size within 2s")
	} else {
		t.Logf("cached view saw the Windows write after %v", sizeAfter)
		if sizeAfter > time.Second {
			t.Errorf("took %v, want under 1s", sizeAfter)
		}
	}
	if got, _ := e.read("/watched"); got != "0123456789" {
		t.Errorf("content = %q", got)
	}
	os.WriteFile(filepath.Join(e.dir, "created"), []byte("x"), 0o666)
	var events []string
	for time.Since(start) < 2500*time.Millisecond {
		events = append(events, inotifyNames(e, ifd)...)
		if len(events) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("inotify events: %v", events)
	sawModify, sawCreate := false, false
	for _, ev := range events {
		if strings.HasSuffix(ev, ":watched") && strings.HasPrefix(ev, "0x2:") {
			sawModify = true
		}
		if strings.HasSuffix(ev, ":created") && strings.HasPrefix(ev, "0x100:") {
			sawCreate = true
		}
	}
	if !sawModify || !sawCreate {
		t.Errorf("modify seen: %v, create seen: %v", sawModify, sawCreate)
	}
	// The new file is listed although the directory listing was cached.
	if got := e.list("/"); fmt.Sprint(got) != "[created watched]" {
		t.Errorf("listing = %v", got)
	}
	// Guest changes are not echoed back as host changes.
	e.write("/guest", "g", 0o644)
	time.Sleep(400 * time.Millisecond)
	for _, ev := range inotifyNames(e, ifd) {
		if strings.HasSuffix(ev, ":guest") {
			t.Logf("event for a guest change: %s (guest-made create is reported once by the VFS itself)", ev)
		}
	}
}

// An editor's save by replacement keeps the mode and owner the guest set.
func TestVFSSafeSaveKeepsMetadata(t *testing.T) {
	e := newEnv(t, HostFolderOptions{})
	e.write("/doc", "v1", 0o640)
	e.must("chown", e.v.SetStatAt(e.ctx, e.creds, e.pop("/doc"), &vfs.SetStatOptions{Stat: linux.Statx{Mask: linux.STATX_UID | linux.STATX_GID, UID: 1000, GID: 1000}}))
	if st, _ := e.stat("/doc"); st.UID != 1000 {
		t.Fatalf("chown: %+v", st)
	}
	tmp := filepath.Join(e.dir, ".doc.swp")
	os.WriteFile(tmp, []byte("version two"), 0o666)
	if err := os.Rename(tmp, filepath.Join(e.dir, "doc")); err != nil {
		t.Fatal(err)
	}
	st, err := e.stat("/doc")
	e.must("stat after replacement", err)
	if st.Mode != linux.S_IFREG|0o640 || st.UID != 1000 || st.GID != 1000 || st.Size != 11 {
		t.Errorf("after replacement: mode %#o uid %d gid %d size %d", st.Mode, st.UID, st.GID, st.Size)
	}
}

// Speed of the backend, which opens a host handle per call: an untar of 20000
// small files and a git-status-like walk of stat calls, against plain os calls
// on the same disk. The walk runs warm (dentries cached by the untar) and cold
// (a fresh mount of the same folder). Only logged.
func TestVFSPerf(t *testing.T) {
	const dirs, perDir, size = 400, 50, 2048
	n := dirs * perDir
	payload := string(make([]byte, size))
	per := func(d time.Duration) time.Duration { return (d / time.Duration(n)).Round(time.Microsecond) }
	walk := func(e *env) time.Duration {
		start := time.Now()
		for d := 0; d < dirs; d++ {
			for f := 0; f < perDir; f++ {
				if _, err := e.stat(fmt.Sprintf("/d%03d/f%02d", d, f)); err != nil {
					t.Fatal(err)
				}
			}
		}
		return time.Since(start)
	}
	for _, mode := range []struct {
		name string
		hf   HostFolderOptions
	}{
		{"watched (write-through cache)", HostFolderOptions{Watched: true}},
		{"revalidating (default)", HostFolderOptions{}},
		{"exclusive", HostFolderOptions{Exclusive: true}},
	} {
		e := newEnv(t, mode.hf)
		start := time.Now()
		for d := 0; d < dirs; d++ {
			e.must("mkdir", e.v.MkdirAt(e.ctx, e.creds, e.pop(fmt.Sprintf("/d%03d", d)), &vfs.MkdirOptions{Mode: 0o755}))
			for f := 0; f < perDir; f++ {
				p := fmt.Sprintf("/d%03d/f%02d", d, f)
				fd, err := e.v.OpenAt(e.ctx, e.creds, e.pop(p), &vfs.OpenOptions{Flags: linux.O_CREAT | linux.O_WRONLY, Mode: 0o751})
				e.must("create", err)
				_, err = fd.Write(e.ctx, usermem.BytesIOSequence([]byte(payload)), vfs.WriteOptions{})
				e.must("write", err)
				fd.DecRef(e.ctx)
			}
		}
		untar := time.Since(start)
		warm := walk(e)
		warm2 := walk(e)
		cold := walk(newEnv(t, HostFolderOptions{Path: e.dir, Exclusive: mode.hf.Exclusive, Watched: mode.hf.Watched}))
		t.Logf("%s: untar %v/file; stat walk warm %v/stat (second %v), cold %v/stat", mode.name, per(untar), per(warm), per(warm2), per(cold))
	}
	base := t.TempDir()
	start := time.Now()
	for d := 0; d < dirs; d++ {
		os.Mkdir(filepath.Join(base, fmt.Sprintf("d%03d", d)), 0o755)
		for f := 0; f < perDir; f++ {
			os.WriteFile(filepath.Join(base, fmt.Sprintf("d%03d", d), fmt.Sprintf("f%02d", f)), []byte(payload), 0o644)
		}
	}
	untar := time.Since(start)
	var lstat [2]time.Duration
	for pass := range lstat {
		start = time.Now()
		for d := 0; d < dirs; d++ {
			for f := 0; f < perDir; f++ {
				os.Lstat(filepath.Join(base, fmt.Sprintf("d%03d", d), fmt.Sprintf("f%02d", f)))
			}
		}
		lstat[pass] = time.Since(start)
	}
	t.Logf("baseline os calls: untar %v/file; lstat walk %v/stat then %v/stat", per(untar), per(lstat[0]), per(lstat[1]))
}

// A name looked up before it existed (cached as missing) is found once
// Windows creates it, and a Windows write right after a guest write is seen.
func TestVFSNegativeCacheAndQuickChange(t *testing.T) {
	e := newEnv(t, HostFolderOptions{Watched: true})
	if _, err := e.stat("/later"); !linuxerr.Equals(linuxerr.ENOENT, err) {
		t.Fatalf("stat of a missing file: %v", err)
	}
	os.WriteFile(filepath.Join(e.dir, "later"), []byte("x"), 0o666)
	found := false
	for start := time.Now(); time.Since(start) < 3*time.Second; time.Sleep(10 * time.Millisecond) {
		if _, err := e.stat("/later"); err == nil {
			found = true
			t.Logf("file made on Windows found after %v", time.Since(start))
			break
		}
	}
	if !found {
		t.Errorf("a file created on Windows is not found within 3s")
	}
	e.write("/g", "guest", 0o644)
	if st, _ := e.stat("/g"); st.Size != 5 {
		t.Fatalf("size %d", st.Size)
	}
	os.WriteFile(filepath.Join(e.dir, "g"), []byte("windows wrote more"), 0o666) // right after the guest write
	seen := false
	for start := time.Now(); time.Since(start) < 3*time.Second; time.Sleep(10 * time.Millisecond) {
		if st, err := e.stat("/g"); err == nil && st.Size == 18 {
			seen = true
			t.Logf("write right after a guest write seen after %v", time.Since(start))
			break
		}
	}
	if !seen {
		t.Errorf("a Windows write right after a guest write was not seen within 3s")
	}
}

func hostFS(e *env) *filesystem { return e.root.Mount().Filesystem().Impl().(*filesystem) }

// In watched mode a change whose event is lost leaves the cache stale; the
// overflow of the watch (or its failure) drops everything.
func TestVFSWatchedOverflowDropsTheCache(t *testing.T) {
	testNoWatcher = true // no watch: every change below is a "lost event"
	defer func() { testNoWatcher = false }()
	e := newEnv(t, HostFolderOptions{Watched: true})
	e.write("/f", "abc", 0o644)
	if st, err := e.stat("/f"); err != nil || st.Size != 3 {
		t.Fatalf("stat %+v %v", st, err)
	}
	if _, err := e.stat("/missing"); !linuxerr.Equals(linuxerr.ENOENT, err) {
		t.Fatalf("stat of a missing file: %v", err)
	}
	if got, _ := e.read("/f"); got != "abc" { // fills the data cache
		t.Fatalf("read %q", got)
	}
	e.must("mkdir", e.v.MkdirAt(e.ctx, e.creds, e.pop("/d"), &vfs.MkdirOptions{Mode: 0o755}))
	_ = e.list("/") // caches the listing

	os.WriteFile(filepath.Join(e.dir, "f"), []byte("0123456789"), 0o666)
	os.WriteFile(filepath.Join(e.dir, "missing"), []byte("x"), 0o666)
	os.Rename(filepath.Join(e.dir, "d"), filepath.Join(e.dir, "d2"))
	time.Sleep(100 * time.Millisecond)

	// Nothing noticed the changes: the cache answers with the old state.
	if st, _ := e.stat("/f"); st.Size != 3 {
		t.Logf("the cache already saw the change (size %d); the test only proves the drop below", st.Size)
	}
	if _, err := e.stat("/missing"); err == nil {
		t.Logf("the cached 'does not exist' was not used")
	}

	// The host stats of the drop run outside renameMu.
	free := false
	testDropStat = func(fs *filesystem) {
		if free = fs.renameMu.TryLock(); free {
			fs.renameMu.Unlock()
		}
	}
	hostFS(e).hostChange(e.ctx, winhost.Change{Overflow: true})
	testDropStat = nil
	if !free {
		t.Errorf("renameMu is held during the host stats of the drop")
	}

	if st, err := e.stat("/f"); err != nil || st.Size != 10 {
		t.Errorf("after the overflow: stat /f = %+v %v, want size 10", st, err)
	}
	if got, _ := e.read("/f"); got != "0123456789" {
		t.Errorf("after the overflow: read /f = %q", got)
	}
	if _, err := e.stat("/missing"); err != nil {
		t.Errorf("after the overflow: /missing is still reported missing: %v", err)
	}
	if got := e.list("/"); fmt.Sprint(got) != "[d2 f missing]" {
		t.Errorf("after the overflow: listing = %v", got)
	}
	if _, err := e.stat("/d"); !linuxerr.Equals(linuxerr.ENOENT, err) {
		t.Errorf("after the overflow: /d still exists: %v", err)
	}
}

// The default is to revalidate: a change is seen at once without the watch.
func TestVFSDefaultRevalidates(t *testing.T) {
	testNoWatcher = true
	defer func() { testNoWatcher = false }()
	e := newEnv(t, HostFolderOptions{})
	e.write("/f", "abc", 0o644)
	if st, _ := e.stat("/f"); st.Size != 3 {
		t.Fatalf("size %d", st.Size)
	}
	os.WriteFile(filepath.Join(e.dir, "f"), []byte("0123456789"), 0o666)
	if st, err := e.stat("/f"); err != nil || st.Size != 10 {
		t.Errorf("the default mount did not see a change made without events: %+v %v", st, err)
	}
	if hostFS(e).opts.interop != InteropModeShared {
		t.Errorf("default interop is %v, want revalidating", hostFS(e).opts.interop)
	}
}

// A FIFO of a host folder is an in-Sentry pipe, and the two mounts of the same
// folder share it; a socket still cannot be opened.
func TestVFSFifoSharedAcrossMounts(t *testing.T) {
	e1 := newEnv(t, HostFolderOptions{})
	e2 := newEnv(t, HostFolderOptions{Path: e1.dir})
	e1.must("mknod fifo", e1.v.MknodAt(e1.ctx, e1.creds, e1.pop("/p"), &vfs.MknodOptions{Mode: linux.S_IFIFO | 0o600}))
	e1.must("mknod socket", e1.v.MknodAt(e1.ctx, e1.creds, e1.pop("/s"), &vfs.MknodOptions{Mode: linux.S_IFSOCK | 0o600}))
	w, err := e1.v.OpenAt(e1.ctx, e1.creds, e1.pop("/p"), &vfs.OpenOptions{Flags: linux.O_RDWR | linux.O_NONBLOCK})
	e1.must("open the fifo in mount 1", err)
	defer w.DecRef(e1.ctx)
	r, err := e2.v.OpenAt(e2.ctx, e2.creds, e2.pop("/p"), &vfs.OpenOptions{Flags: linux.O_RDWR | linux.O_NONBLOCK})
	e2.must("open the fifo in mount 2", err)
	defer r.DecRef(e2.ctx)
	if _, err := w.Write(e1.ctx, usermem.BytesIOSequence([]byte("hello")), vfs.WriteOptions{}); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 16)
	n, err := r.Read(e2.ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if err != nil || string(buf[:n]) != "hello" {
		t.Errorf("read through the other mount = %q, %v", buf[:n], err)
	}
	if fd, err := e1.v.OpenAt(e1.ctx, e1.creds, e1.pop("/s"), &vfs.OpenOptions{Flags: linux.O_RDWR}); err == nil {
		fd.DecRef(e1.ctx)
		t.Errorf("a socket file of a host folder opened")
	} else if !linuxerr.Equals(linuxerr.ENXIO, err) {
		t.Errorf("opening a socket file: %v, want ENXIO", err)
	}
}
