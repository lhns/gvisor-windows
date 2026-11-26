// gvisor-windows: Linux ABI shim, see docs/adr/0006.

//go:build windows && amd64

package unix

import (
	"io/fs"
	"strconv"
	"syscall"
)

// Errno is a Linux errno value (not a Windows error code). Its numbering and
// messages come from the linux/amd64 errorList in zerrors_linux_amd64_shim_windows.go.
type Errno uintptr

func (e Errno) Error() string {
	for _, x := range errorList {
		if x.num == e {
			return x.desc
		}
	}
	return "errno " + strconv.Itoa(int(e))
}

func (e Errno) Is(target error) bool {
	switch target {
	case fs.ErrPermission:
		return e == EACCES || e == EPERM
	case fs.ErrExist:
		return e == EEXIST || e == ENOTEMPTY
	case fs.ErrNotExist:
		return e == ENOENT
	}
	return false
}

func (e Errno) Temporary() bool {
	return e == EINTR || e == EMFILE || e == ENFILE || e.Timeout()
}

func (e Errno) Timeout() bool {
	return e == EAGAIN || e == EWOULDBLOCK || e == ETIMEDOUT
}

// Signal is a Linux signal number.
type Signal int

func (s Signal) Signal() {}

func (s Signal) String() string {
	for _, x := range signalList {
		if x.num == s {
			return x.desc
		}
	}
	return "signal " + strconv.Itoa(int(s))
}

// The raw system call layer does not exist on Windows: every call fails with ENOSYS.

func Syscall(trap, a1, a2, a3 uintptr) (r1, r2 uintptr, err Errno) { return 0, 0, ENOSYS }

func Syscall6(trap, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err Errno) {
	return 0, 0, ENOSYS
}

func RawSyscall(trap, a1, a2, a3 uintptr) (r1, r2 uintptr, err Errno) { return 0, 0, ENOSYS }

func RawSyscall6(trap, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err Errno) {
	return 0, 0, ENOSYS
}

func SyscallNoError(trap, a1, a2, a3 uintptr) (r1, r2 uintptr) { return 0, 0 }

func RawSyscallNoError(trap, a1, a2, a3 uintptr) (r1, r2 uintptr) { return 0, 0 }

// SysProcAttr is Windows' own; the Linux fields are not available.
type SysProcAttr = syscall.SysProcAttr

func gettimeofday(tv *Timeval) (err Errno) { return ENOSYS }
