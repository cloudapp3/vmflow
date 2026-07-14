//go:build windows

package controlapi

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func preserveConfigMetadata(temp *os.File, tempPath, targetPath string, targetInfo os.FileInfo) error {
	if err := temp.Chmod(targetInfo.Mode()); err != nil {
		return err
	}
	securityInformation := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION |
		windows.GROUP_SECURITY_INFORMATION |
		windows.DACL_SECURITY_INFORMATION)
	descriptor, err := windows.GetNamedSecurityInfo(targetPath, windows.SE_FILE_OBJECT, securityInformation)
	if err != nil {
		return err
	}
	if descriptor == nil {
		return fmt.Errorf("target file security descriptor is unavailable")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	group, _, err := descriptor.Group()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		tempPath,
		windows.SE_FILE_OBJECT,
		securityInformation,
		owner,
		group,
		dacl,
		nil,
	)
}

func replaceConfigFile(source, target string) error {
	sourcePtr, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		sourcePtr,
		targetPtr,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

// MoveFileEx with MOVEFILE_WRITE_THROUGH flushes the rename operation before
// returning. Windows does not expose a portable directory fsync operation.
func syncConfigDirectory(_ string) error {
	return nil
}
