// gvisor-windows: on Linux, mounts are served through gofer processes.

//go:build !windows

package boot

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/gofer"
)

func hostFolderForRoot(spec *specs.Spec) (gofer.HostFolderOptions, bool) {
	return gofer.HostFolderOptions{}, false
}

func hostFolderForMount(m *specs.Mount) (gofer.HostFolderOptions, bool) {
	return gofer.HostFolderOptions{}, false
}
