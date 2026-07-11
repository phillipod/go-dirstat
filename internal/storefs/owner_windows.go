//go:build windows

package storefs

import (
	"encoding/binary"
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
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return false
	}
	if windows.EqualSid(owner, user.User.Sid) {
		return true
	}
	// Windows commonly reports BUILTIN\\Administrators as the owner of runner
	// and service-created directories. Treat that owner as current-user-owned
	// only when the effective token is actually a member of the group; the
	// private DACL check still rejects untrusted write/delete grants.
	if !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		return false
	}
	member, err := token.IsMember(owner)
	return err == nil && member
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
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil || ace == nil {
			return false
		}
		sid, ok := aceSID(ace)
		if !ok {
			return false
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE || ace.Header.AceType == accessDeniedObjectACEType || ace.Header.AceType == accessDeniedCallbackACEType {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE && ace.Header.AceType != accessAllowedObjectACEType && ace.Header.AceType != accessAllowedCallbackACEType {
			// Unknown ACE layouts are not safe to interpret as an allow grant.
			return false
		}
		allowed := windows.EqualSid(sid, user.User.Sid) ||
			sid.IsWellKnown(windows.WinLocalSystemSid) ||
			sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) ||
			sid.IsWellKnown(windows.WinCreatorOwnerSid)
		if !allowed && ace.Mask&untrustedWriteMask != 0 {
			return false
		}
	}
	return true
}

const (
	accessAllowedObjectACEType   = 5
	accessDeniedObjectACEType    = 6
	accessAllowedCallbackACEType = 9
	accessDeniedCallbackACEType  = 10
	fileDeleteChild              = 0x00000040

	// A read/execute grant from an inherited Users or Application Packages
	// ACE is normal on Windows. Reject only rights that can modify, delete, or
	// re-ACL the store; generic/max/all masks are included because they can be
	// expanded by the OS into those rights.
	untrustedWriteMask = windows.FILE_GENERIC_WRITE |
		windows.DELETE |
		windows.WRITE_DAC |
		windows.WRITE_OWNER |
		windows.GENERIC_WRITE |
		windows.GENERIC_ALL |
		windows.MAXIMUM_ALLOWED |
		windows.ACCESS_SYSTEM_SECURITY |
		fileDeleteChild
)

// aceSID returns the trustee SID for the allow/deny ACE layouts used in a
// DACL. GetAce exposes every ACE through ACCESS_ALLOWED_ACE for historical API
// reasons, so object ACEs need their optional GUID fields skipped before the
// SID can be interpreted. Bounds and SID-length checks keep malformed data
// fail-closed rather than allowing an unsafe pointer read.
func aceSID(ace *windows.ACCESS_ALLOWED_ACE) (*windows.SID, bool) {
	if ace == nil || ace.Header.AceSize < 8 {
		return nil, false
	}
	raw := unsafe.Slice((*byte)(unsafe.Pointer(ace)), int(ace.Header.AceSize))
	offset := 8 // ACE_HEADER (4) + ACCESS_MASK (4)
	if ace.Header.AceType == accessAllowedObjectACEType || ace.Header.AceType == accessDeniedObjectACEType {
		if len(raw) < offset+4 {
			return nil, false
		}
		flags := binary.LittleEndian.Uint32(raw[offset : offset+4])
		offset += 4
		if flags&windows.ACE_OBJECT_TYPE_PRESENT != 0 {
			offset += 16
		}
		if flags&windows.ACE_INHERITED_OBJECT_TYPE_PRESENT != 0 {
			offset += 16
		}
	}
	if offset >= len(raw) {
		return nil, false
	}
	sid := (*windows.SID)(unsafe.Pointer(&raw[offset]))
	if len(raw)-offset < 8 || !sid.IsValid() {
		return nil, false
	}
	return sid, true
}

func makePrivateStore(path string, _ func(fs.FileMode) error) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	userSID := user.User.Sid.String()
	// Set the owner explicitly as well as protecting the DACL from inheritance.
	// Hosted Windows runners can create temporary directories whose owner is an
	// administrators group rather than the process account; normalizing both
	// fields makes the current-user ownership contract deterministic. Grant
	// full control only to the current user, LocalSystem, and builtin
	// Administrators. OICI propagates the same private boundary to
	// cache/history files and key directories.
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
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		user.User.Sid,
		nil,
		dacl,
		nil,
	)
}
