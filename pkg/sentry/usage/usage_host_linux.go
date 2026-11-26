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

//go:build linux

package usage

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/memutil"
)

// allocStats returns the shared memory backing RTMemoryStats.
func allocStats() (*os.File, uintptr, error) {
	const name = "memory-usage"
	fd, err := memutil.CreateMemFD(name, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("error creating usage file: %v", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if err := file.Truncate(int64(RTMemoryStatsSize)); err != nil {
		return nil, 0, fmt.Errorf("error truncating usage file: %v", err)
	}
	// Note: We rely on the returned page being initially zeroed. This will
	// always be the case for a newly mapped page from /dev/shm. If we obtain
	// the shared memory through some other means in the future, we may have to
	// explicitly zero the page.
	mmap, err := memutil.MapFile(0, RTMemoryStatsSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED, file.Fd(), 0)
	if err != nil {
		return nil, 0, fmt.Errorf("error mapping usage file: %v", err)
	}
	return file, mmap, nil
}
