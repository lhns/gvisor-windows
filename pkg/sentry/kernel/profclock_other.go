//go:build !windows

package kernel

import "time"

func profNow() int64 { return time.Now().UnixNano() }
