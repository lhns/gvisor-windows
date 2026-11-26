//go:build windows

package winhost

import (
	"golang.org/x/sys/unix"
	"golang.org/x/sys/windows"
)

// SetCaseSensitive makes a directory case sensitive (it must be empty) or not.
//
// NTFS wants FILE_DELETE_CHILD on the directory for this (measured: write
// attributes, DELETE, WRITE_DAC, even MAXIMUM_ALLOWED are not enough, and
// `fsutil` fails the same way). A folder in the user's profile gives that
// right; a folder that was only granted Modify does not. The owner of a
// directory may always change its ACL, so when the call is refused the
// current user is granted FILE_DELETE_CHILD on this one directory (not
// inherited) and the call is repeated. No administrator is needed.
func (fs *FS) SetCaseSensitive(dir ID, on bool) error {
	err := fs.setCaseSensitiveOnce(dir, on)
	if err == unix.EACCES && on {
		if gerr := fs.grantDeleteChild(dir); gerr == nil {
			err = fs.setCaseSensitiveOnce(dir, on)
		}
	}
	return err
}

func (fs *FS) setCaseSensitiveOnce(dir ID, on bool) error {
	h, err := fs.openID(dir, fileReadAttributes|fileWriteAttributes)
	if err != nil {
		return err
	}
	defer fs.closeID(dir, h)
	return setCaseSensitive(h, on)
}

// grantDeleteChild adds an ACE for the current user giving FILE_DELETE_CHILD
// on the directory itself.
func (fs *FS) grantDeleteChild(dir ID) error {
	const readControl, writeDAC = 0x20000, 0x40000
	h, err := ntOpenByID(fs.root, dir, readControl|writeDAC, shareAll, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return toErrno(err)
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return toErrno(err)
	}
	old, _, err := sd.DACL()
	if err != nil {
		return toErrno(err)
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: fileDeleteChild,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(tu.User.Sid),
		},
	}}, old)
	if err != nil {
		return toErrno(err)
	}
	return toErrno(windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil))
}
