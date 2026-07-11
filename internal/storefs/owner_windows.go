//go:build windows

package storefs

import (
	"io/fs"
	"unsafe"

	"golang.org/x/sys/windows"
)

func ownedByCurrentUser(path string, _ fs.FileInfo) bool {
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return false
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return false
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && user != nil && user.User.Sid != nil && windows.EqualSid(owner, user.User.Sid)
}

func privateStore(path string, info fs.FileInfo) bool {
	if !ownedByCurrentUser(path, info) {
		return false
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return false
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return false
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return false
	}
	trusted := []*windows.SID{user.User.Sid}
	for _, sidType := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinLocalSystemSid,
		windows.WinBuiltinAdministratorsSid,
		windows.WinCreatorOwnerSid,
	} {
		sid, sidErr := windows.CreateWellKnownSid(sidType)
		if sidErr != nil {
			return false
		}
		trusted = append(trusted, sid)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil || ace == nil {
			return false
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			// Object/callback ACE layouts carry the SID at different offsets.
			// Fail closed instead of mis-parsing a potentially broad grant.
			return false
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		allowed := false
		for _, trustedSID := range trusted {
			if windows.EqualSid(sid, trustedSID) {
				allowed = true
				break
			}
		}
		if !allowed && ace.Mask != 0 {
			return false
		}
	}
	return true
}

func makePrivateStore(path string, _ func(fs.FileMode) error) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	userSID := user.User.Sid.String()
	// Protect the DACL from inheritance and grant full control only to the
	// current user, LocalSystem, and builtin Administrators. OICI propagates
	// the same private boundary to cache/history files and key directories.
	descriptor, err := windows.SecurityDescriptorFromString(
		"D:P(A;OICI;FA;;;" + userSID + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)",
	)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}
