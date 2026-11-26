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

//go:build windows

package specutils

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/moby/sys/capability"
)

// gvisor-windows: the Windows host has no namespaces to set up (ADR 0008).
// namespace.go holds the Linux code; these are the parts the sandbox calls.

// GetNS is the same as the Linux version: it only reads the spec.
func GetNS(nst specs.LinuxNamespaceType, s *specs.Spec) (specs.LinuxNamespace, bool) {
	if s.Linux == nil {
		return specs.LinuxNamespace{}, false
	}
	for _, ns := range s.Linux.Namespaces {
		if ns.Type == nst {
			return ns, true
		}
	}
	return specs.LinuxNamespace{}, false
}

// HasCapabilities is always false: Linux capabilities do not exist on the host.
func HasCapabilities(cs ...capability.Cap) bool { return false }

// KnownNamespaces returns the namespace types the sandbox itself implements.
func KnownNamespaces() []string {
	return []string{
		string(specs.CgroupNamespace),
		string(specs.IPCNamespace),
		string(specs.MountNamespace),
		string(specs.NetworkNamespace),
		string(specs.PIDNamespace),
		string(specs.UserNamespace),
		string(specs.UTSNamespace),
	}
}
