//go:build windows

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

const windowsFileDeleteChild windows.ACCESS_MASK = 0x00000040

const windowsDangerousWriteMask windows.ACCESS_MASK = windows.GENERIC_ALL |
	windows.GENERIC_WRITE |
	windows.WRITE_DAC |
	windows.WRITE_OWNER |
	windows.DELETE |
	windows.ACCESS_MASK(windows.FILE_WRITE_DATA) |
	windows.ACCESS_MASK(windows.FILE_APPEND_DATA) |
	windows.ACCESS_MASK(windows.FILE_WRITE_ATTRIBUTES) |
	windows.ACCESS_MASK(windows.FILE_WRITE_EA) |
	windowsFileDeleteChild

func validateTrustedServiceBinary(path string, _ os.FileInfo) error {
	parent := filepath.Dir(path)
	if windowsIsRoot(parent) {
		return fmt.Errorf("service binary %s is not trusted: binary must be under a protected install directory, not directly in %s", path, parent)
	}

	for _, objectPath := range windowsTrustedPathComponents(path) {
		if err := validateTrustedWindowsObject(path, objectPath); err != nil {
			return err
		}
	}
	return nil
}

func windowsTrustedPathComponents(path string) []string {
	components := []string{path}
	for dir := filepath.Dir(path); !windowsIsRoot(dir); dir = filepath.Dir(dir) {
		components = append(components, dir)
	}
	return components
}

func windowsIsRoot(path string) bool {
	clean := filepath.Clean(path)
	return filepath.Dir(clean) == clean
}

func validateTrustedWindowsObject(binaryPath, objectPath string) error {
	sd, err := windows.GetNamedSecurityInfo(
		objectPath,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("service binary %s is not trusted: inspect ACL for %s: %w", binaryPath, objectPath, err)
	}
	if sd == nil {
		return fmt.Errorf("service binary %s is not trusted: %s has no security descriptor", binaryPath, objectPath)
	}

	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("service binary %s is not trusted: inspect owner for %s: %w", binaryPath, objectPath, err)
	}
	if owner == nil || !windowsTrustedPrincipal(owner) {
		return fmt.Errorf("service binary %s is not trusted: %s is owned by %s, not Administrators/SYSTEM/TrustedInstaller", binaryPath, objectPath, windowsSIDLabel(owner))
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("service binary %s is not trusted: inspect DACL for %s: %w", binaryPath, objectPath, err)
	}
	// A nil DACL grants full access to everyone.
	if dacl == nil {
		return fmt.Errorf("service binary %s is not trusted: %s has a nil DACL", binaryPath, objectPath)
	}

	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("service binary %s is not trusted: read ACE %d for %s: %w", binaryPath, i, objectPath, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if ace.Mask&windowsDangerousWriteMask == 0 {
			continue
		}

		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !windowsTrustedPrincipal(sid) {
			return fmt.Errorf("service binary %s is not trusted: %s grants write access to %s", binaryPath, objectPath, windowsSIDLabel(sid))
		}
	}
	return nil
}

func windowsTrustedPrincipal(sid *windows.SID) bool {
	if sid == nil {
		return false
	}
	if sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) ||
		sid.IsWellKnown(windows.WinLocalSystemSid) ||
		sid.IsWellKnown(windows.WinCreatorOwnerSid) {
		return true
	}
	if sid.String() == trustedInstallerSID {
		return true
	}
	account, domain, _, err := sid.LookupAccount("")
	if err != nil {
		return false
	}
	return strings.EqualFold(domain, "NT SERVICE") && strings.EqualFold(account, "TrustedInstaller")
}

func windowsSIDLabel(sid *windows.SID) string {
	if sid == nil {
		return "<nil>"
	}
	account, domain, _, err := sid.LookupAccount("")
	if err == nil {
		if strings.TrimSpace(domain) != "" {
			return domain + `\` + account
		}
		return account
	}
	return sid.String()
}
