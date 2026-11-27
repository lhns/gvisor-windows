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

package safecopy

import (
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sighandling"
)

// gvisor-windows: moved here unchanged from safecopy.go.
var (
	// savedSigSegVHandler is a pointer to the SIGSEGV handler that was
	// configured before we replaced it with our own. We still call into it
	// when we get a SIGSEGV that is not interesting to us.
	savedSigSegVHandler uintptr

	// Same as above, but for SIGBUS signals.
	savedSigBusHandler uintptr
)

// signalHandler is our replacement signal handler for SIGSEGV and SIGBUS
// signals.
func signalHandler()

// addrOfSignalHandler returns the start address of signalHandler.
//
// See comment on addrOfMemcpy for more details.
func addrOfSignalHandler() uintptr

func installFaultHandlers() {
	if err := sighandling.ReplaceSignalHandler(unix.SIGSEGV, addrOfSignalHandler(), &savedSigSegVHandler); err != nil {
		panic(fmt.Sprintf("Unable to set handler for SIGSEGV: %v", err))
	}
	if err := sighandling.ReplaceSignalHandler(unix.SIGBUS, addrOfSignalHandler(), &savedSigBusHandler); err != nil {
		panic(fmt.Sprintf("Unable to set handler for SIGBUS: %v", err))
	}
}


// ensureFaultStack is a no-op on Linux, where signal handlers run on the
// signal stack.
func ensureFaultStack() {}
