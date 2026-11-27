# 1n: sweep for Go memory handed to Windows as uintptr

Scope: every Windows-specific Go file outside third_party (winhost, pgalloc, safecopy, winnat, runsc, tools/windows, test/spikes/windows, winstub). Method: grep for `uintptr(unsafe.Pointer`, `...uintptr` wrappers, overlapped/async APIs and attribute lists, plus `GOOS=windows go vet`.

Background: `LazyProc.Call` (x/sys/windows and syscall) is `//go:uintptrescapes`, so `proc.Call(uintptr(unsafe.Pointer(&x)))` written inline is safe: x is moved to the heap and kept alive. The bug is a uintptr stored first and passed later, or passed through a wrapper without that directive.

| Where | Pattern | Verdict | Fix |
|---|---|---|---|
| pkg/safecopy (faults in goroutines) | (e) exception dispatch runs on the goroutine stack; CONTEXT with AVX-512 state plus ntdll dispatch can exceed the runtime's headroom and overwrite neighbouring heap | real, probable cause of the `negative WaitGroup counter` in sentrysmoke probeSafecopy (line 188 is the deferred `wg.Done`; no stray `Done` exists, so the counter was corrupted) | `ensureFaultStack` (16 KB frame, assembly, no body) before each fault-capable asm call; docs/results/1b recorded it as prototyped and dropped, restored from 0b1700df8 |
| test/spikes/windows/s7/main.go:379 | (a) `evp` = uintptr of a stack array, reused across a loop of WaitForDebugEvent | real | VirtualAlloc buffer, as s8 already does |
| s7/main.go:68, s8/main.go:81 (`utf16p`) | (a) returns uintptr of a heap string; unreferenced before the call | real (GC window) | returns `*uint16`, converted inline at the call |
| s4/main.go:99-105 (`dp`, `rp`) | (a) uintptr of slice data stored, passed later | real | kept as `unsafe.Pointer`, converted inline |
| s7/main.go:140,200 (`vehMark`) | (a) uintptr of a MapViewOfFile view | false positive (not Go memory) | none |
| pkg/sentry/platform/winstub (`call`, `ctxBuf`, `debugEvent`) | (a)(b) | already fixed in 1l (heap buffers, `//go:uintptrescapes`); not touched | none |
| winhost/watch_windows.go:67-96 | (c) OVERLAPPED and buffer used by an async ReadDirectoryChangesW across goroutine waits | false positive: `ov` is captured by the watcher closure and the buffer is a 64 KB heap slice, so neither moves | none (file left alone) |
| winhost/host_windows.go:980,1013; index_windows.go:170 | (c) OVERLAPPED for offset reads/writes | false positive: synchronous handles, completes inside the call | none |
| winhost/nt_windows.go:258 | (a) inline `uintptr(unsafe.Pointer(&iosb))`, `&buf[0])` in `Call` | false positive (inline, uintptrescapes) | none |
| tools/windows/runsctest/tty_windows.go:52, tools/windows/podman/ttydrive/main_windows.go:59 | (d) `unsafe.Pointer(c.hpc)`; vet "possible misuse" (reported twice) | false positive: the PSEUDOCONSOLE attribute value is the HPCON handle, not memory; the attribute list is allocated by x/sys outside Go memory | one-line comment |
| tools/windows/sentrysmoke/main.go:132-158 | (d) `unsafe.Pointer(good/bad)` from VirtualAlloc | false positive | one-line comment |
| pkg/sentry/pgalloc/pgalloc_host_windows.go:267,341; pgalloc_unsafe.go:29 | (d) conversions of VirtualAlloc/section-view addresses | false positive; QueryWorkingSetEx `entries` is a heap slice converted inline | one-line comment |
| pkg/safecopy tests (faultmatrix, safecopy_test, testmap) | (d) uintptr from mapped memory | false positive | none |
| tools/windows/sentryguest, sentrysmoke, bench, disasm, spikes s1-s2, s5, s8; winnat; runsc/winrunsc, boot/*_windows.go | inline `Call` conversions, typed x/sys APIs, no stored uintptr or async pointer | false positive / clean | none |

Verdict: one real stack-corruption candidate (safecopy fault dispatch on small goroutine stacks, the likely cause of the one-off WaitGroup panic, fixed but only provable by CI soak) and three small stored-uintptr cases in spikes; the production Windows code outside winstub (already fixed) is otherwise clean.
