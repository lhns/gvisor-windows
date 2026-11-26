// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pgalloc

import (
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// gvisor-windows: mincore moved to mincore_linux.go (Windows: mincore_windows.go).

// Preconditions: The FileRange represented by c is a superset of fr.
func (c *chunkInfo) sliceAt(fr memmap.FileRange) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(c.mapping+uintptr(fr.Start&chunkMask))), fr.Length())
}

func canMergeIovecAndSlice(iov unix.Iovec, bs []byte) bool {
	return uintptr(unsafe.Pointer(iov.Base))+uintptr(iov.Len) == uintptr(unsafe.Pointer(unsafe.SliceData(bs)))
}
