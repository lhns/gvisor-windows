//go:build windows

package winhost

import (
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Changes the backend makes itself show up in the directory-change stream and
// must not be reported as changes made from Windows. They are recognized by
// content, not by a time window, so a real change right after one is kept:
//
//   - creating, linking, unlinking and renaming leave a token (kind, name)
//     that the one matching Added/Removed/Renamed event consumes;
//   - writes and attribute changes record the state they leave the file in
//     (last write time and size, read after the handle is closed, because
//     NTFS updates the time on close); a Modified event is ours if the file
//     still is in exactly that state.
//
// A Windows write to the same file changes the time or the size, so it is
// seen. The weak points: an event of ours that never arrives leaves a token
// that hides one later Added/Removed event of the same name (tokens expire
// after a minute), and a Windows write that leaves the same time and size
// is missed.

type tokKind uint8

const (
	tokAppeared tokKind = iota
	tokGone
)

type tokKey struct {
	kind tokKind
	name string
}

type tokVal struct {
	n int
	t time.Time
}

type sig struct {
	mtime int64
	size  int64
}

type ownChanges struct {
	mu   sync.Mutex
	toks map[tokKey]tokVal
	sigs map[ID]sig
}

const (
	maxOwnTokens = 8192
	maxOwnSigs   = 16384
	tokenTTL     = time.Minute
)

func (fs *FS) expect(kind tokKind, name string) {
	if !fs.watching.Load() {
		return
	}
	o := &fs.shadow.own
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.toks == nil {
		o.toks = map[tokKey]tokVal{}
	}
	if len(o.toks) > maxOwnTokens {
		now := time.Now()
		for k, v := range o.toks {
			if now.Sub(v.t) > tokenTTL {
				delete(o.toks, k)
			}
		}
	}
	k := tokKey{kind, name}
	v := o.toks[k]
	o.toks[k] = tokVal{n: v.n + 1, t: time.Now()}
}

func (fs *FS) consume(kind tokKind, name string) bool {
	o := &fs.shadow.own
	o.mu.Lock()
	defer o.mu.Unlock()
	k := tokKey{kind, name}
	v, ok := o.toks[k]
	if !ok || time.Since(v.t) > tokenTTL {
		delete(o.toks, k)
		return false
	}
	if v.n <= 1 {
		delete(o.toks, k)
	} else {
		o.toks[k] = tokVal{n: v.n - 1, t: v.t}
	}
	return true
}

// noteSelf records the state our own change left the file in.
func (fs *FS) noteSelf(id ID) {
	if !fs.watching.Load() {
		return
	}
	h, err := ntOpenByID(fs.root, id, fileReadAttributes, shareAll, 0)
	if err != nil {
		return
	}
	var bi fileBasicInfo
	var si fileStandardInfo
	ok := getInfo(h, classBasic, unsafe.Pointer(&bi), uint32(unsafe.Sizeof(bi))) == nil &&
		getInfo(h, classStandard, unsafe.Pointer(&si), uint32(unsafe.Sizeof(si))) == nil
	windows.CloseHandle(h)
	if !ok {
		return
	}
	o := &fs.shadow.own
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sigs == nil {
		o.sigs = map[ID]sig{}
	}
	if len(o.sigs) > maxOwnSigs {
		n := 0
		for k := range o.sigs {
			delete(o.sigs, k)
			if n++; n > maxOwnSigs/2 {
				break
			}
		}
	}
	o.sigs[id] = sig{mtime: bi.LastWriteTime, size: si.EndOfFile}
}

func (fs *FS) isSelfSig(id ID, cur sig) bool {
	o := &fs.shadow.own
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.sigs[id]
	return ok && s == cur
}
