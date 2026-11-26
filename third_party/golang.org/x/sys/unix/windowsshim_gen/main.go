// gvisor-windows: Linux ABI shim, see docs/adr/0006.
//
// Command windowsshim_gen regenerates the *_shim_windows.go files in the
// parent directory from the linux/amd64 files of the same x/sys version:
// the build constraint becomes windows, syscall.Errno/Signal become the
// shim's own types, and the few calls into package syscall that do not exist
// on Windows are replaced by ENOSYS stubs.
//
// Run from the unix directory: go run ./windowsshim_gen
package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Linux/amd64 files that are copied; the output name is <base>_shim_windows.go.
var files = []string{
	"affinity_linux.go", "auxv.go", "bluetooth_linux.go", "constants.go",
	"dev_linux.go", "dirent.go", "env_unix.go", "fcntl.go", "fdset.go",
	"ifreq_linux.go", "ioctl_linux.go", "ioctl_unsigned.go", "mremap.go", "pagesize_unix.go",
	"race0.go", "readdirent_getdents.go", "readv_unix.go", "sockcmsg_linux.go",
	"sockcmsg_unix.go", "sockcmsg_unix_other.go", "syscall.go", "syscall_linux.go",
	"syscall_linux_alarm.go", "syscall_linux_amd64.go", "syscall_unix.go", "sysvshm_linux.go",
	"sysvshm_unix.go", "timestruct.go", "zerrors_linux.go", "zerrors_linux_amd64.go",
	"zptrace_x86_linux.go", "zsyscall_linux.go", "zsyscall_linux_amd64.go",
	"zsysnum_linux_amd64.go", "ztypes_linux.go", "ztypes_linux_amd64.go",
}

// Source-text patches, applied to every file.
var patches = []struct{ re, repl string }{
	{`syscall\.Errno`, `Errno`},
	{`syscall\.Signal`, `Signal`},
	{`syscall\.(EAGAIN|EINVAL|ENOENT)\b`, `$1`},
	// Package syscall on Windows has none of these.
	{`(?s)//go:linkname syscall_prlimit syscall\.prlimit\nfunc syscall_prlimit\([^\n]*\n\n`, ``},
	{`return syscall_prlimit\([^\n]*\)`, `return ENOSYS`},
	{`return syscall\.(Setuid|Setgid|Setreuid|Setregid|Setresuid|Setresgid|Exec|Setrlimit)\([^\n]*\)`, `return ENOSYS`},
}

var buildLine = regexp.MustCompile(`(?m)^//go:build .*\n|^// \+build .*\n`)
var syscallUse = regexp.MustCompile(`(?m)^[^/\n]*\bsyscall\.[A-Za-z]`)
var syscallImport = regexp.MustCompile(`(?m)^\t?"syscall"\n|^import "syscall"\n\n?`)

func main() {
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			panic(err)
		}
		s := buildLine.ReplaceAllString(string(b), "")
		for _, p := range patches {
			s = regexp.MustCompile(p.re).ReplaceAllString(s, p.repl)
		}
		if !syscallUse.MatchString(s) {
			s = syscallImport.ReplaceAllString(s, "")
		}
		// Header and constraint go before the first line of the file.
		out := "// gvisor-windows: Linux ABI shim, see docs/adr/0006. Generated from " + f + " by windowsshim_gen.\n\n" +
			"//go:build windows && amd64\n\n" + s
		name := strings.TrimSuffix(f, ".go") + "_shim_windows.go"
		if err := os.WriteFile(name, []byte(out), 0o644); err != nil {
			panic(err)
		}
	}
	fmt.Println("generated", len(files), "files")
}
