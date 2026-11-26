//go:build windows

package winhost

import "sync"

// A file that Windows replaces (an editor's write-temp-then-rename "safe
// save", git checkout) gets a new file ID and no metadata stream, so the
// guest would see the mount defaults for mode, owner and xattrs. The backend
// remembers the metadata it last saw for each (directory, name) and, when a
// lookup finds a different regular file without a stream at that name, writes
// the remembered metadata to it. The memory is per run: a replacement that
// happens while no sandbox is running, or of a file the guest never looked
// at, is not covered (a persistent per-folder index would be needed).

type shadowKey struct {
	dir  ID
	name string
}

type shadowEntry struct {
	id   ID
	meta Metadata
}

const (
	maxShadow   = 100000
)

type shadowState struct {
	mu    sync.Mutex
	files map[shadowKey]shadowEntry
	byID  map[ID]shadowKey

	// The persisted log (index_windows.go).
	pending   []indexRec
	indexSize int64
	stopIdx   chan struct{}
	idxDone   chan struct{}

	// Changes the backend makes itself produce directory-change events that
	// must not be reported as Windows changes. Names and IDs of recent own
	// changes are kept for selfWindow.
	own ownChanges
}

func (fs *FS) remember(dir ID, name string, id ID, m *Metadata) {
	s := &fs.shadow
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[shadowKey]shadowEntry{}
	}
	if len(s.files) >= maxShadow {
		n := 0
		for k := range s.files {
			delete(s.files, k)
			if n++; n >= maxShadow/2 {
				break
			}
		}
	}
	k := shadowKey{dir, name}
	if s.byID == nil {
		s.byID = map[ID]shadowKey{}
	}
	s.byID[id] = k
	if old, ok := s.files[k]; ok && sameEntry(old, id, m) {
		return
	}
	c := *m
	s.files[k] = shadowEntry{id: id, meta: c}
	s.queueIndex(indexRec{dir: dir, name: name, id: id, meta: c})
}

// touchShadow updates the remembered metadata of a file whose metadata the
// backend just changed (chmod, chown, xattrs), under whatever name it has.
func (fs *FS) touchShadow(id ID, m *Metadata) {
	if m.Mode&SIFMT != SIFREG {
		return
	}
	s := &fs.shadow
	s.mu.Lock()
	k, ok := s.byID[id]
	s.mu.Unlock()
	if ok {
		fs.remember(k.dir, k.name, id, m)
	}
}

// forget drops the memory of a name the guest removed or renamed.
func (fs *FS) forget(dir ID, name string) {
	s := &fs.shadow
	s.mu.Lock()
	if _, ok := s.files[shadowKey{dir, name}]; ok {
		delete(s.files, shadowKey{dir, name})
		s.queueIndex(indexRec{del: true, dir: dir, name: name})
	}
	s.mu.Unlock()
}

// noteMeta is called with what a lookup found. It remembers a stream, or
// restores the remembered metadata of a replaced file.
func (fs *FS) noteMeta(dir ID, name string, a Attr, m *Metadata) Attr {
	if m != nil {
		if m.Mode&SIFMT == SIFREG {
			fs.remember(dir, name, a.ID, m)
		}
		return a
	}
	if a.Mode&SIFMT != SIFREG || a.Native&(attrReparse|attrSystem|attrDirectory) != 0 {
		return a
	}
	s := &fs.shadow
	s.mu.Lock()
	e, ok := s.files[shadowKey{dir, name}]
	s.mu.Unlock()
	if !ok || e.id == a.ID {
		return a
	}
	m2 := e.meta
	m2.Mode = SIFREG | e.meta.Mode&0o7777
	m2.RdevMajor, m2.RdevMinor, m2.Target = 0, 0, ""
	defer fs.noteSelf(a.ID)
	h, err := fs.openID(a.ID, xattrAccess)
	if err != nil {
		return a
	}
	defer fs.closeID(a.ID, h)
	fs.metaMu.Lock()
	err = fs.writeMeta(h, &m2)
	fs.metaMu.Unlock()
	if err != nil {
		return a
	}
	fs.remember(dir, name, a.ID, &m2)
	a.Mode, a.UID, a.GID = m2.Mode, m2.UID, m2.GID
	return a
}

