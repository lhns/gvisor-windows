// gvisor-windows: on Windows the Sentry opens the rootfs folder of the OCI
// bundle and bind-mounted folders itself (ADR 0005); there are no gofers.

//go:build windows

package boot

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/gofer"
)

// RootfsOptionsAnnotation holds the comma-separated folder options of the
// rootfs (see hostFolderOptions).
const RootfsOptionsAnnotation = "dev.gvisor.windows.rootfs-options"

func hostFolderForRoot(spec *specs.Spec) (gofer.HostFolderOptions, bool) {
	if spec.Root == nil || spec.Root.Path == "" {
		return gofer.HostFolderOptions{}, false
	}
	// A root that is a file is an erofs image, mounted through GoferFDs.
	if fi, err := os.Stat(spec.Root.Path); err == nil && !fi.IsDir() {
		return gofer.HostFolderOptions{}, false
	}
	// The root has no mount options in an OCI spec; this annotation carries
	// them, e.g. "file-mode=0755" for a folder whose files Windows created.
	var opts []string
	if v := spec.Annotations[RootfsOptionsAnnotation]; v != "" {
		opts = strings.Split(v, ",")
	}
	return hostFolderOptions(spec.Root.Path, opts), true
}

func hostFolderForMount(m *specs.Mount) (gofer.HostFolderOptions, bool) {
	if m.Source == "" {
		return gofer.HostFolderOptions{}, false
	}
	return hostFolderOptions(m.Source, m.Options), true
}

// isHostFolderOption reports whether o is an option of hostFolderOptions.
func isHostFolderOption(o string) bool {
	switch o {
	case "emulate-symlinks", "case-sensitive", "cache=exclusive", "cache=watched":
		return true
	}
	for _, p := range []string{"owner=", "file-mode=", "dir-mode="} {
		if strings.HasPrefix(o, p) {
			return true
		}
	}
	return false
}

// hostFolderOptions reads the options of a folder mount: owner=UID:GID (the
// owner of files without stored metadata, default 0:0), emulate-symlinks,
// case-sensitive, cache=watched (trust the change watch, faster, with holes)
// and cache=exclusive. The default revalidates on every use.
func hostFolderOptions(path string, options []string) gofer.HostFolderOptions {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	hf := gofer.HostFolderOptions{Path: path}
	for _, o := range options {
		switch {
		case strings.HasPrefix(o, "owner="):
			if u, g, ok := strings.Cut(strings.TrimPrefix(o, "owner="), ":"); ok {
				uid, e1 := strconv.ParseUint(u, 10, 32)
				gid, e2 := strconv.ParseUint(g, 10, 32)
				if e1 == nil && e2 == nil {
					hf.UID, hf.GID = uint32(uid), uint32(gid)
				}
			}
		case strings.HasPrefix(o, "file-mode="), strings.HasPrefix(o, "dir-mode="):
			k, v, _ := strings.Cut(o, "=")
			if m, err := strconv.ParseUint(v, 8, 32); err == nil {
				if k == "file-mode" {
					hf.FileMode = uint32(m)
				} else {
					hf.DirMode = uint32(m)
				}
			}
		case o == "emulate-symlinks":
			hf.EmulateSymlinks = true
		case o == "case-sensitive":
			hf.CaseSensitive = true
		case o == "cache=exclusive":
			hf.Exclusive = true
		case o == "cache=watched":
			hf.Watched = true
		}
	}
	return hf
}
