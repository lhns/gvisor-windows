//go:build windows

package winhost

import (
	"bytes"
	"encoding/binary"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The memory that lets a replaced file get its metadata back (shadow_windows.go)
// is also kept on disk, in the stream ":gvisor.index" of the folder's root
// directory, so a replacement made while no sandbox runs is covered too. The
// stream is an append-only log of records, written in batches once a second
// and when the mount is closed:
//
//	u8 op (1 = put, 2 = delete)
//	16 bytes directory file ID, u16 name length, name
//	put only: 16 bytes file ID, u32 length, metadata stream (see meta.go)
//
// The log is read when the folder is mounted and rewritten when it holds
// many more records than live entries.

const indexStream = ":gvisor.index"

type indexRec struct {
	del  bool
	dir  ID
	name string
	id   ID
	meta Metadata
}

func appendRec(b []byte, r *indexRec) []byte {
	if r.del {
		b = append(b, 2)
	} else {
		b = append(b, 1)
	}
	b = append(b, r.dir.File[:]...)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(r.name)))
	b = append(b, r.name...)
	if !r.del {
		b = append(b, r.id.File[:]...)
		enc := r.meta.Encode()
		b = binary.LittleEndian.AppendUint32(b, uint32(len(enc)))
		b = append(b, enc...)
	}
	return b
}

// parseIndex applies the records in b to files, returning the number of
// records read.
func parseIndex(b []byte, vol uint64, files map[shadowKey]shadowEntry) int {
	n := 0
	for len(b) >= 1+16+2 {
		op := b[0]
		var dir ID
		dir.Vol = vol
		copy(dir.File[:], b[1:17])
		nl := int(binary.LittleEndian.Uint16(b[17:19]))
		b = b[19:]
		if len(b) < nl {
			break
		}
		name := string(b[:nl])
		b = b[nl:]
		key := shadowKey{dir, name}
		if op == 2 {
			delete(files, key)
			n++
			continue
		}
		if len(b) < 20 {
			break
		}
		id := ID{Vol: vol}
		copy(id.File[:], b[:16])
		ml := int(binary.LittleEndian.Uint32(b[16:20]))
		b = b[20:]
		if len(b) < ml {
			break
		}
		m, err := DecodeMetadata(b[:ml])
		b = b[ml:]
		if err != nil {
			break
		}
		if len(files) < maxShadow {
			files[key] = shadowEntry{id: id, meta: *m}
		}
		n++
	}
	return n
}

// loadIndex reads the persisted memory. Called by Open.
func (fs *FS) loadIndex() {
	s := &fs.shadow
	s.files = map[shadowKey]shadowEntry{}
	h, err := ntOpen(fs.root, indexStream, fileReadData|fileReadAttributes, shareAll, windows.FILE_OPEN, 0, 0)
	if err != nil {
		return
	}
	var si fileStandardInfo
	if getInfo(h, classStandard, unsafe.Pointer(&si), uint32(unsafe.Sizeof(si))) != nil || si.EndOfFile <= 0 || si.EndOfFile > 256<<20 {
		windows.CloseHandle(h)
		return
	}
	buf := make([]byte, si.EndOfFile)
	var n uint32
	err = windows.ReadFile(h, buf, &n, nil)
	windows.CloseHandle(h)
	if err != nil {
		return
	}
	records := parseIndex(buf[:n], fs.vol, s.files)
	s.indexSize = int64(n)
	if records > 2*len(s.files)+1000 {
		fs.rewriteIndex()
	}
}

// rewriteIndex replaces the log by one put per live entry.
func (fs *FS) rewriteIndex() {
	s := &fs.shadow
	var b []byte
	for k, e := range s.files {
		b = appendRec(b, &indexRec{dir: k.dir, name: k.name, id: e.id, meta: e.meta})
	}
	h, err := ntOpen(fs.root, indexStream, fileWriteData|fileReadAttributes, shareAll, windows.FILE_OVERWRITE_IF, 0, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	var n uint32
	if windows.WriteFile(h, b, &n, nil) == nil {
		s.indexSize = int64(n)
	}
}

// queueIndex adds a record to the next batch. s.mu must be held.
func (s *shadowState) queueIndex(r indexRec) {
	s.pending = append(s.pending, r)
}

// flushIndex appends the queued records to the stream.
func (fs *FS) flushIndex() {
	s := &fs.shadow
	s.mu.Lock()
	recs := s.pending
	s.pending = nil
	size := s.indexSize
	s.mu.Unlock()
	if len(recs) == 0 {
		return
	}
	var b []byte
	for i := range recs {
		b = appendRec(b, &recs[i])
	}
	h, err := ntOpen(fs.root, indexStream, fileWriteData|fileReadAttributes, shareAll, windows.FILE_OPEN_IF, 0, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	ov := windows.Overlapped{Offset: uint32(size), OffsetHigh: uint32(size >> 32)}
	var n uint32
	if windows.WriteFile(h, b, &n, &ov) == nil {
		s.mu.Lock()
		s.indexSize = size + int64(n)
		s.mu.Unlock()
	}
}

func (fs *FS) startIndexFlusher() {
	s := &fs.shadow
	s.stopIdx = make(chan struct{})
	s.idxDone = make(chan struct{})
	go func() {
		defer close(s.idxDone)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				fs.flushIndex()
			case <-s.stopIdx:
				fs.flushIndex()
				return
			}
		}
	}()
}

func (fs *FS) stopIndexFlusher() {
	s := &fs.shadow
	if s.stopIdx != nil {
		close(s.stopIdx)
		<-s.idxDone
		s.stopIdx = nil
	}
}

func sameEntry(e shadowEntry, id ID, m *Metadata) bool {
	return e.id == id && bytes.Equal(e.meta.Encode(), m.Encode())
}
