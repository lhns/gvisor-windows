package kernel

import (
	"syscall"
	"unsafe"
)

var (
	pQPC    = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
	pQPF    = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceFrequency")
	pCPU    = syscall.NewLazyDLL("kernel32.dll").NewProc("GetCurrentProcessorNumber")
	qpcFreq float64
)

// profCPU returns the processor the caller runs on.
func profCPU() int32 {
	r, _, _ := pCPU.Call()
	return int32(r)
}

// profNow returns a high-resolution monotonic time in ns (Go's clock ticks only every 0.5 to 15 ms on Windows).
func profNow() int64 {
	if qpcFreq == 0 {
		var f int64
		pQPF.Call(uintptr(unsafe.Pointer(&f)))
		qpcFreq = float64(f)
	}
	var c int64
	pQPC.Call(uintptr(unsafe.Pointer(&c)))
	return int64(float64(c) / qpcFreq * 1e9)
}
