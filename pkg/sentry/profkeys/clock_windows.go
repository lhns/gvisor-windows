package profkeys

import (
	"syscall"
	"unsafe"
)

var (
	pQPC    = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
	pQPF    = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceFrequency")
	qpcFreq float64
)

func now() int64 {
	if qpcFreq == 0 {
		var f int64
		pQPF.Call(uintptr(unsafe.Pointer(&f)))
		qpcFreq = float64(f)
	}
	var c int64
	pQPC.Call(uintptr(unsafe.Pointer(&c)))
	return int64(float64(c) / qpcFreq * 1e9)
}
