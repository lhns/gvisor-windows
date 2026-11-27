// Copyright 2026 The gVisor Authors.
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

//go:build windows && amd64

package winstub

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"

	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
)

// Name is the platform's name in runsc's --platform flag.
const Name = "winstub"

// Platform implements platform.Platform with stub processes.
//
// Address range. Guest addresses run from stubasm.GuestMin (0x400000, above the
// regions Windows maps at fixed low addresses and below which the trap table,
// trampolines and veneers live) up to stubasm.GuestMax (0x6FFF_8000_0000, where the
// per-thread control pages start). Two holes remain inside: KUSER_SHARED_DATA at
// 0x7FFE0000 and one more page below 0x7FFEC000; the platform cannot express
// holes, so the first Switch in an address space reserves [0x7FFE0000,0x7FFF0000)
// as a sealed PROT_NONE mapping in the memory manager.
//
// Threads. Every stub process has one debugger goroutine, locked to its OS thread
// (Windows delivers debug events only to the thread that created the process); it
// handles faults and unpatched syscalls briefly and never blocks on the Sentry.
// Each platform context owns one stub thread; the task goroutine that calls
// Switch waits for the thread's request on its own.
type Platform struct {
	platform.NoCPUPreemptionDetection
	platform.NoCPUNumbers

	scans         scanCache
	veneers       veneerRegistry
	active        atomic.Int32 // contexts inside Switch
	tokens        atomic.Int32 // spinning tokens held (policyTokens)
	maxTokens     int32
	denied        atomic.Int32 // contexts that found no token free since the last handover
	spinLimit     int          // spin only while active <= spinLimit (WINSTUB_SPINLIMIT)
	xmask         uint64
	fsRewrite     bool // %fs instructions are rewritten (CPU without FSGSBASE or forced)
	fpSize        int
	stubPath      string
	image         []byte
	ntSet, ntWait uint64
	ntTerm        uint64
}

// New creates the platform.
func New(opts platform.Options) (*Platform, error) {
	// ADR 0020: guests of CPUs without FSGSBASE get their %fs instructions
	// rewritten; WINSTUB_FSMODE=native|rewrite forces a mode.
	hasFS := stubasm.HasFSGSBASE()
	rewrite := !hasFS
	switch m := os.Getenv("WINSTUB_FSMODE"); m {
	case "":
	case "rewrite":
		rewrite = true
	case "native":
		if !hasFS {
			return nil, errors.New("winstub: WINSTUB_FSMODE=native, but the CPU lacks FSGSBASE")
		}
		rewrite = false
	default:
		return nil, fmt.Errorf("winstub: WINSTUB_FSMODE=%q: want native or rewrite", m)
	}
	if err := pNtSetEvent.Find(); err != nil {
		return nil, err
	}
	if err := pNtWaitSingle.Find(); err != nil {
		return nil, err
	}
	if err := pNtTerminateThread.Find(); err != nil {
		return nil, err
	}
	// Without XSAVE (or with WINSTUB_FPMODE=fxsave, which also hides it from the
	// guest, see pkg/cpuid) the stub saves the FP state with fxsave: 512 bytes.
	fxsave := !cpuid.HostFeatureSet().UseXsave()
	fpSize, _ := cpuid.HostFeatureSet().ExtendedStateSize()
	fpSize -= cpuid.HostFeatureSet().AMXExtendedStateSize()
	switch m := os.Getenv("WINSTUB_FPMODE"); m {
	case "", "xsave":
		if fxsave && m != "" {
			return nil, errors.New("winstub: WINSTUB_FPMODE=xsave, but the CPU lacks XSAVE")
		}
	case "fxsave":
		if !fxsave {
			return nil, errors.New("winstub: WINSTUB_FPMODE=fxsave, but the CPU feature set still has XSAVE")
		}
	default:
		return nil, fmt.Errorf("winstub: WINSTUB_FPMODE=%q: want xsave or fxsave", m)
	}
	if fpSize > stubasm.XsaveSize {
		return nil, fmt.Errorf("winstub: xsave area of %d bytes does not fit", fpSize)
	}
	f, err := os.CreateTemp("", "winstub-*.exe")
	if err != nil {
		return nil, fmt.Errorf("winstub: stub image: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(stubImage()); err != nil {
		return nil, fmt.Errorf("winstub: stub image: %w", err)
	}
	return &Platform{
		spinLimit: envInt("WINSTUB_SPINLIMIT", runtime.NumCPU()),
		maxTokens: int32(envInt("WINSTUB_TOKENS", max(1, runtime.NumCPU()/2))),
		xmask:    stubasm.XSaveMask(),
		fsRewrite: rewrite,
		fpSize:   int(fpSize),
		stubPath: f.Name(),
		image:    stubasm.Image(rewrite, fxsave),
		ntSet:    uint64(pNtSetEvent.Addr()),
		ntWait:   uint64(pNtWaitSingle.Addr()),
		ntTerm:   uint64(pNtTerminateThread.Addr()),
	}, nil
}

// Close removes the stub image file (best effort: Windows keeps it while a stub
// runs).
func (p *Platform) Close() { os.Remove(p.stubPath) }

// SupportsAddressSpaceIO implements platform.Platform.
func (*Platform) SupportsAddressSpaceIO() bool { return false }

// HaveGlobalMemoryBarrier implements platform.Platform. FlushProcessWriteBuffers
// only covers the Sentry's own threads, not the stubs.
func (*Platform) HaveGlobalMemoryBarrier() bool { return false }

// GlobalMemoryBarrier implements platform.Platform.
func (*Platform) GlobalMemoryBarrier() error { return errUnsupported }

// MapUnit implements platform.Platform: views are mapped as whole ranges.
func (*Platform) MapUnit() uint64 { return 0 }

// MinUserAddress implements platform.Platform.
func (*Platform) MinUserAddress() hostarch.Addr { return hostarch.Addr(stubasm.GuestMin) }

// MaxUserAddress implements platform.Platform.
func (*Platform) MaxUserAddress() hostarch.Addr { return hostarch.Addr(stubasm.GuestMax) }

// NewAddressSpace implements platform.Platform. A new stub process takes about
// 20 ms to start. opts.DisableSyscallPatching is ignored: an unpatched syscall
// would reach the Windows kernel.
func (p *Platform) NewAddressSpace(opts platform.AddressSpaceOptions) (platform.AddressSpace, error) {
	as, err := p.newAS()
	if err != nil {
		return nil, err
	}
	return as, nil
}

// NewContext implements platform.Platform.
func (p *Platform) NewContext(pkgcontext.Context) platform.Context {
	c := &winContext{}
	c.intr.Dst = c
	return c
}

// SeccompInfo implements platform.Platform.
func (*Platform) SeccompInfo() platform.SeccompInfo {
	return platform.StaticSeccompInfo{PlatformName: Name}
}

// ConcurrencyCount implements platform.Platform.
func (*Platform) ConcurrencyCount() int { return runtime.NumCPU() }

type constructor struct{}

func (constructor) New(opts platform.Options) (platform.Platform, error) { return New(opts) }

func (constructor) PrecompiledSeccompInfo() []platform.SeccompInfo { return nil }

func init() { platform.Register(Name, constructor{}) }
