//go:build windows

package winhost

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ChangeAction is what happened to a path.
type ChangeAction int

const (
	Added ChangeAction = iota + 1
	Removed
	Modified
	RenamedFrom
	RenamedTo
)

// Change is a change made to the folder from outside the backend.
type Change struct {
	// Path is relative to the mounted folder, slash separated, with Linux
	// names (unescaped).
	Path   string
	Action ChangeAction
	// Overflow says events were lost: everything cached may be stale.
	Overflow bool
}

type watcher struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// Watch reports changes made to the folder by others (ReadDirectoryChangesW
// over the whole tree). Changes the backend makes itself are left out. fn is
// called from a single goroutine and must not block for long. Watch can be
// called once.
func (fs *FS) Watch(fn func(Change)) error {
	lp := string([]rune{92, 92, 63, 92}) // the long-path prefix
	p, err := windows.UTF16PtrFromString(lp + strings.TrimPrefix(fs.path, lp))
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, 0x1 /*FILE_LIST_DIRECTORY*/, shareAll, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return fmt.Errorf("watch: opening the folder: %w", err)
	}
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return fmt.Errorf("watch: creating an event: %w", err)
	}
	w := &watcher{stop: make(chan struct{}), done: make(chan struct{})}
	backing := make([]uint64, 8*1024)
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&backing[0])), len(backing)*8)
	const filter = 0x1 | 0x2 | 0x4 | 0x8 | 0x10 | 0x40 // names, attributes, size, last write, creation
	ov := windows.Overlapped{HEvent: ev}
	// The folder only collects changes while a read is pending, so the first
	// read is issued before Watch returns, and each later read is issued
	// before the previous result is processed (a change made in between would
	// be lost otherwise).
	arm := func() error {
		windows.ResetEvent(ev)
		return windows.ReadDirectoryChanges(h, &buf[0], uint32(len(buf)), true, filter, nil, &ov, 0)
	}
	if err := arm(); err != nil {
		windows.CloseHandle(ev)
		windows.CloseHandle(h)
		return fmt.Errorf("watch: first read: %w", err)
	}
	fs.watcher = w
	fs.watching.Store(true)
	go func() {
		defer close(w.done)
		defer windows.CloseHandle(ev)
		defer windows.CloseHandle(h)
		for {
			for {
				r, _ := windows.WaitForSingleObject(ev, 100)
				if r == windows.WAIT_OBJECT_0 {
					break
				}
				select {
				case <-w.stop:
					windows.CancelIoEx(h, &ov)
					windows.GetOverlappedResult(h, &ov, new(uint32), true)
					return
				default:
				}
			}
			var n uint32
			if err := windows.GetOverlappedResult(h, &ov, &n, false); err != nil {
				return
			}
			data := append([]byte(nil), buf[:n]...)
			if err := arm(); err != nil {
				return
			}
			if n == 0 {
				fn(Change{Overflow: true})
				continue
			}
			fs.dispatch(data, fn)
		}
	}()
	return nil
}

func (fs *FS) stopWatch() {
	if w := fs.watcher; w != nil {
		w.once.Do(func() { close(w.stop) })
		select {
		case <-w.done:
		case <-time.After(2 * time.Second):
		}
	}
}

func (fs *FS) dispatch(buf []byte, fn func(Change)) {
	off := 0
	for off+12 <= len(buf) {
		e := buf[off:]
		next := int(*(*uint32)(unsafe.Pointer(&e[0])))
		action := *(*uint32)(unsafe.Pointer(&e[4]))
		nl := int(*(*uint32)(unsafe.Pointer(&e[8])))
		if 12+nl <= len(e) {
			w := make([]uint16, nl/2)
			for i := range w {
				w[i] = *(*uint16)(unsafe.Pointer(&e[12+2*i]))
			}
			fs.deliver(string(utf16.Decode(w)), action, fn)
		}
		if next == 0 {
			break
		}
		off += next
	}
}

func (fs *FS) deliver(rel string, action uint32, fn func(Change)) {
	parts := strings.Split(rel, `\`)
	leaf := UnescapeName(parts[len(parts)-1])
	if leaf == "" || strings.Contains(parts[len(parts)-1], ":") {
		return // alternate stream event
	}
	c := Change{Action: ChangeAction(action)}
	for i := range parts {
		parts[i] = UnescapeName(parts[i])
	}
	c.Path = strings.Join(parts, "/")
	switch c.Action {
	case Added, RenamedTo:
		if fs.consume(tokAppeared, leaf) {
			return
		}
	case Removed, RenamedFrom:
		if fs.consume(tokGone, leaf) {
			return
		}
	}
	if c.Action == Modified {
		// Directory timestamps change with every entry: only files count.
		id, cur, isDir, ok := fs.modifiedState(rel)
		if !ok || isDir || fs.isSelfSig(id, cur) {
			return
		}
		// It may be our own write whose state is not recorded yet (the event
		// can arrive before the writer has read the file back): look again
		// shortly, once per path.
		fs.cbMu.Lock()
		if fs.recheck == nil {
			fs.recheck = map[string]bool{}
		}
		if fs.recheck[rel] {
			fs.cbMu.Unlock()
			return
		}
		fs.recheck[rel] = true
		fs.cbMu.Unlock()
		time.AfterFunc(40*time.Millisecond, func() {
			fs.cbMu.Lock()
			delete(fs.recheck, rel)
			fs.cbMu.Unlock()
			id2, cur2, isDir2, ok2 := fs.modifiedState(rel)
			if !ok2 || isDir2 || fs.isSelfSig(id2, cur2) {
				return
			}
			fs.cbMu.Lock()
			defer fs.cbMu.Unlock()
			fn(c)
		})
		return
	}
	fs.cbMu.Lock()
	defer fs.cbMu.Unlock()
	fn(c)
}

// modifiedState reads the ID, last write time and size of the file at rel.
func (fs *FS) modifiedState(rel string) (id ID, cur sig, isDir, ok bool) {
	h, err := ntOpen(fs.root, rel, fileReadAttributes, shareAll, windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	var bi fileBasicInfo
	var idi fileIDInfo
	var si fileStandardInfo
	ok = getInfo(h, classBasic, unsafe.Pointer(&bi), uint32(unsafe.Sizeof(bi))) == nil &&
		getInfo(h, classStandard, unsafe.Pointer(&si), uint32(unsafe.Sizeof(si))) == nil &&
		getInfo(h, classIDInfo, unsafe.Pointer(&idi), uint32(unsafe.Sizeof(idi))) == nil
	return ID{Vol: idi.VolumeSerialNumber, File: idi.FileID}, sig{mtime: bi.LastWriteTime, size: si.EndOfFile}, bi.FileAttributes&attrDirectory != 0, ok
}

// ResolvePath returns the ID of the file at a slash-separated path relative
// to the folder (names in Linux form), without following a final symlink.
func (fs *FS) ResolvePath(rel string) (ID, error) {
	cur := fs.rootID
	if rel == "" {
		return cur, nil
	}
	for _, n := range strings.Split(rel, "/") {
		a, err := fs.Lookup(cur, n)
		if err != nil {
			return ID{}, err
		}
		cur = a.ID
	}
	return cur, nil
}
