// gvisor-windows: reading the extended attributes stored in an image.

package erofs

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

func (i *inode) xattrAllowed(creds *auth.Credentials, name string) error {
	mode := linux.FileMode(i.Mode())
	kuid := auth.KUID(i.UID())
	if vfs.XattrReadNeedsFilePermission(name) {
		if err := vfs.GenericCheckPermissions(creds, vfs.MayRead, mode, nil, kuid, auth.KGID(i.GID())); err != nil {
			return err
		}
	}
	return vfs.CheckXattrPermissions(creds, vfs.MayRead, mode, kuid, name)
}

func (i *inode) listXattr(creds *auth.Credentials) ([]string, error) {
	xs, err := i.Xattrs()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, x := range xs {
		if i.xattrAllowed(creds, x.Name) == nil {
			names = append(names, x.Name)
		}
	}
	return names, nil
}

func (i *inode) getXattr(creds *auth.Credentials, name string) (string, error) {
	if err := i.xattrAllowed(creds, name); err != nil {
		return "", err
	}
	v, err := i.GetXattr(name)
	if err != nil {
		if linuxerr.Equals(linuxerr.ENODATA, err) {
			return "", linuxerr.ENODATA
		}
		return "", err
	}
	return string(v), nil
}
