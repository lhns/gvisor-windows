//go:build windows

package rootfs

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	"gvisor.dev/gvisor/pkg/sentry/uniqueid"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

type testClock struct{}

func (testClock) Now() ktime.Time                     { return ktime.FromNanoseconds(time.Now().UnixNano()) }
func (testClock) NewTimer(ktime.Listener) ktime.Timer { panic("no timers in tests") }

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
	case uniqueid.CtxInotifyCookie:
		return uint32(1)
	case limits.CtxLimits:
		return limits.NewLimitSet()
	}
	return c.Context.Value(key)
}

type env struct {
	t     *testing.T
	ctx   *testContext
	v     *vfs.VirtualFilesystem
	creds *auth.Credentials
	root  vfs.VirtualDentry
}

func mount(t *testing.T, image string) *env {
	t.Helper()
	mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{DisableIMAWorkAround: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := &testContext{Context: context.Background(), mf: mf}
	v := &vfs.VirtualFilesystem{}
	if err := v.Init(ctx); err != nil {
		t.Fatal(err)
	}
	Register(v)
	creds := auth.NewRootCredentials(auth.NewRootUserNamespace())
	start := time.Now()
	fd, err := OpenImage(image)
	if err != nil {
		t.Fatal(err)
	}
	mns, err := NewMountNamespace(ctx, v, creds, fd)
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	t.Logf("%s: mounted in %v", filepath.Base(image), time.Since(start))
	ctx.mns = mns
	return &env{t: t, ctx: ctx, v: v, creds: creds, root: mns.Root(ctx)}
}

func (e *env) pop(p string, follow bool) *vfs.PathOperation {
	return &vfs.PathOperation{Root: e.root, Start: e.root, Path: fspath.Parse(p), FollowFinalSymlink: follow}
}

func (e *env) lstat(p string) (linux.Statx, error) {
	return e.v.StatAt(e.ctx, e.creds, e.pop(p, false), &vfs.StatOptions{Mask: linux.STATX_BASIC_STATS})
}

func (e *env) readAll(p string) ([]byte, error) {
	fd, err := e.v.OpenAt(e.ctx, e.creds, e.pop(p, true), &vfs.OpenOptions{Flags: linux.O_RDONLY})
	if err != nil {
		return nil, err
	}
	defer fd.DecRef(e.ctx)
	var out []byte
	buf := make([]byte, 1<<20)
	for off := int64(0); ; {
		n, err := fd.PRead(e.ctx, usermem.BytesIOSequence(buf), off, vfs.ReadOptions{})
		out = append(out, buf[:n]...)
		off += n
		if n == 0 || err != nil {
			if err != nil && err.Error() != "EOF" && n == 0 && off == 0 {
				return nil, err
			}
			return out, nil
		}
	}
}

func (e *env) readdir(p string) ([]string, error) {
	fd, err := e.v.OpenAt(e.ctx, e.creds, e.pop(p, true), &vfs.OpenOptions{Flags: linux.O_RDONLY | linux.O_DIRECTORY})
	if err != nil {
		return nil, err
	}
	defer fd.DecRef(e.ctx)
	var names []string
	err = fd.IterDirents(e.ctx, vfs.IterDirentsCallbackFunc(func(d vfs.Dirent) error {
		if d.Name != "." && d.Name != ".." {
			names = append(names, d.Name)
		}
		return nil
	}))
	return names, err
}

// TestImages checks every image in $GVISOR_EROFS_DIR against the manifest that
// the Linux job made from the tree the image was built from.
func TestImages(t *testing.T) {
	dir := os.Getenv("GVISOR_EROFS_DIR")
	if dir == "" {
		t.Skip("GVISOR_EROFS_DIR is not set")
	}
	images, _ := filepath.Glob(filepath.Join(dir, "*.erofs"))
	if len(images) == 0 {
		t.Fatalf("no images in %s", dir)
	}
	for _, img := range images {
		img := img
		name := strings.TrimSuffix(filepath.Base(img), ".erofs")
		t.Run(name, func(t *testing.T) { checkImage(t, img, strings.TrimSuffix(img, ".erofs")) })
	}
}

func checkImage(t *testing.T, img, base string) {
	fi, _ := os.Stat(img)
	t.Logf("image size %d MiB", fi.Size()>>20)
	e := mount(t, img)

	// The manifest: path, raw mode (hex), uid, gid, size, rdev major, minor
	// (hex), nlink, inode.
	f, err := os.Open(base + ".manifest")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	inoGroup := map[string]uint64{} // source inode -> guest inode
	var n, bad, links int
	counts := map[uint32]int{}
	start := time.Now()
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) != 9 {
			t.Fatalf("bad manifest line %q", sc.Text())
		}
		path := strings.TrimPrefix(fields[0], ".")
		if path == "" {
			path = "/"
		}
		rawMode, _ := strconv.ParseUint(fields[1], 16, 32)
		uid, _ := strconv.ParseUint(fields[2], 10, 32)
		gid, _ := strconv.ParseUint(fields[3], 10, 32)
		size, _ := strconv.ParseUint(fields[4], 10, 64)
		maj, _ := strconv.ParseUint(fields[5], 16, 32)
		min, _ := strconv.ParseUint(fields[6], 16, 32)
		nlink, _ := strconv.ParseUint(fields[7], 10, 32)
		st, err := e.lstat(path)
		n++
		if err != nil {
			bad++
			if bad < 20 {
				t.Errorf("%s: %v", path, err)
			}
			continue
		}
		ftype := uint32(rawMode) & linux.S_IFMT
		counts[ftype]++
		ok := st.Mode == uint16(rawMode) && uint64(st.UID) == uid && uint64(st.GID) == gid
		if ftype == linux.S_IFREG {
			ok = ok && st.Size == size
		}
		if ftype == linux.S_IFCHR || ftype == linux.S_IFBLK {
			ok = ok && uint64(st.RdevMajor) == maj && uint64(st.RdevMinor) == min
		}
		if ftype != linux.S_IFDIR {
			ok = ok && uint64(st.Nlink) == nlink
		}
		if !ok {
			bad++
			if bad < 20 {
				t.Errorf("%s: got mode %#o uid %d gid %d size %d rdev %d:%d nlink %d, want %#o %d %d %d %x:%x %d",
					path, st.Mode, st.UID, st.GID, st.Size, st.RdevMajor, st.RdevMinor, st.Nlink, rawMode, uid, gid, size, maj, min, nlink)
			}
		}
		if nlink > 1 && ftype != linux.S_IFDIR {
			links++
			if prev, seen := inoGroup[fields[8]]; seen && prev != st.Ino {
				bad++
				t.Errorf("%s: hard link has inode %d, other name has %d", path, st.Ino, prev)
			}
			inoGroup[fields[8]] = st.Ino
		}
	}
	t.Logf("%d entries checked in %v (dirs %d, files %d, symlinks %d, char %d, block %d, fifo %d, sock %d, hard-linked names %d), %d mismatches",
		n, time.Since(start), counts[linux.S_IFDIR], counts[linux.S_IFREG], counts[linux.S_IFLNK], counts[linux.S_IFCHR],
		counts[linux.S_IFBLK], counts[linux.S_IFIFO], counts[linux.S_IFSOCK], links, bad)

	// Symlink targets.
	if lf, err := os.Open(base + ".links"); err == nil {
		defer lf.Close()
		sc := bufio.NewScanner(lf)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		cnt := 0
		for sc.Scan() {
			p, target, _ := strings.Cut(sc.Text(), "\t")
			got, err := e.v.ReadlinkAt(e.ctx, e.creds, e.pop(strings.TrimPrefix(p, "."), false))
			if err != nil || got != target {
				t.Errorf("readlink %s = %q, %v; want %q", p, got, err, target)
			}
			cnt++
		}
		t.Logf("%d symlink targets checked", cnt)
	}

	// File contents.
	if hf, err := os.Open(base + ".hashes"); err == nil {
		defer hf.Close()
		sc := bufio.NewScanner(hf)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		cnt, total := 0, 0
		start := time.Now()
		for sc.Scan() {
			sum, p, _ := strings.Cut(sc.Text(), "  ")
			data, err := e.readAll(strings.TrimPrefix(p, "."))
			if err != nil {
				t.Errorf("read %s: %v", p, err)
				continue
			}
			h := sha256.Sum256(data)
			if hex.EncodeToString(h[:]) != sum {
				t.Errorf("%s: content hash differs (%d bytes)", p, len(data))
			}
			cnt++
			total += len(data)
		}
		t.Logf("%d files read and hashed (%d MiB) in %v", cnt, total>>20, time.Since(start))
	}

	// Large directory listing.
	if names, err := e.readdir("/"); err != nil || len(names) == 0 {
		t.Errorf("root listing: %v %v", names, err)
	}
	if big, err := os.ReadFile(base + ".bigdir"); err == nil {
		p, cnt, _ := strings.Cut(strings.TrimSpace(string(big)), "\t")
		want, _ := strconv.Atoi(cnt)
		names, err := e.readdir(strings.TrimPrefix(p, "."))
		if err != nil || len(names) != want {
			t.Errorf("large directory %s: %d entries, %v; want %d", p, len(names), err, want)
		}
	}

	// Writes land in the overlay.
	e.overlayChecks(t)
}

func (e *env) overlayChecks(t *testing.T) {
	create := func(p, data string) {
		fd, err := e.v.OpenAt(e.ctx, e.creds, e.pop(p, true), &vfs.OpenOptions{Flags: linux.O_CREAT | linux.O_RDWR | linux.O_TRUNC, Mode: 0o600})
		if err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
		defer fd.DecRef(e.ctx)
		if _, err := fd.Write(e.ctx, usermem.BytesIOSequence([]byte(data)), vfs.WriteOptions{}); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	if err := e.v.MkdirAt(e.ctx, e.creds, e.pop("/overlay-test", false), &vfs.MkdirOptions{Mode: 0o755}); err != nil {
		t.Fatalf("mkdir in overlay: %v", err)
	}
	create("/overlay-test/new", "hello")
	if got, err := e.readAll("/overlay-test/new"); err != nil || string(got) != "hello" {
		t.Errorf("new file: %q %v", got, err)
	}
	// Modify a lower file: copy-up.
	var lower string
	for _, c := range []string{"/etc/os-release", "/etc/passwd", "/etc/hostname", "/synth/file-0"} {
		if _, err := e.lstat(c); err == nil {
			lower = c
			break
		}
	}
	if lower != "" {
		before, _ := e.readAll(lower)
		create(lower, "changed")
		if got, _ := e.readAll(lower); string(got) != "changed" {
			t.Errorf("copy-up of %s: %q", lower, got)
		}
		t.Logf("copy-up of %s (%d bytes before) ok", lower, len(before))
		// Unlink a lower file: whiteout.
		if err := e.v.UnlinkAt(e.ctx, e.creds, e.pop(lower, false)); err != nil {
			t.Errorf("unlink lower file: %v", err)
		}
		if _, err := e.lstat(lower); !linuxerr.Equals(linuxerr.ENOENT, err) {
			t.Errorf("lower file after unlink: %v", err)
		}
		names, _ := e.readdir(filepath.ToSlash(filepath.Dir(lower)))
		for _, n := range names {
			if "/"+n == lower || filepath.ToSlash(filepath.Dir(lower))+"/"+n == lower {
				t.Errorf("whited-out %s still listed", lower)
			}
		}
	}
	if err := e.v.SymlinkAt(e.ctx, e.creds, e.pop("/overlay-test/ln", false), "new"); err != nil {
		t.Errorf("symlink in overlay: %v", err)
	}
	if err := e.v.MknodAt(e.ctx, e.creds, e.pop("/overlay-test/null", false), &vfs.MknodOptions{Mode: linux.S_IFCHR | 0o666, DevMajor: 1, DevMinor: 3}); err != nil {
		t.Errorf("mknod in overlay: %v", err)
	}
	fmt.Fprint(os.Stderr, "")
}
