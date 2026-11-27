//go:build !windows

package kernel

import "time"

func profNow() int64 { return time.Now().UnixNano() }

// profCPU returns the processor the caller runs on, or -1.
func profCPU() int32 { return -1 }
