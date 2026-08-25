//go:build windows

package transcription

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestPrivateTempDirectoryHasProtectedSingleUserDACL(t *testing.T) {
	t.Parallel()

	tempDir, err := makePrivateTempDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)
	descriptor, err := windows.GetNamedSecurityInfo(
		tempDir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("security descriptor control = %#x, want protected DACL", control)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil || dacl.AceCount == 0 {
		t.Fatalf("DACL = %#v, want current-user ACEs", dacl)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	hasInheritedChildren := false
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.Equals(user.User.Sid) {
			t.Fatalf("ACE %d grants a principal other than the current user", index)
		}
		if ace.Header.AceFlags&(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) != 0 {
			hasInheritedChildren = true
		}
	}
	if !hasInheritedChildren {
		t.Fatal("DACL does not propagate the current-user restriction to child files")
	}
}
