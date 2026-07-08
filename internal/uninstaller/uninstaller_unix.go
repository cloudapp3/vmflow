//go:build linux || darwin

package uninstaller

import (
	"fmt"
	"io"
	"os"
	"slices"
)

// unixProtected are filesystem roots whose removal would be catastrophic. A
// config-referenced path equal to one of these is refused.
var unixProtected = []string{
	"/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib64",
	"/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/tmp",
	"/usr", "/var",
}

func isProtectedPath(clean string) bool {
	return slices.Contains(unixProtected, clean)
}

// removeSelf deletes the running binary. On Linux and macOS deleting an in-use
// executable only unlinks the directory entry; the inode is reclaimed once the
// process exits, so the command can finish and return normally.
func removeSelf(path string, w io.Writer) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove binary: %w", err)
	}
	fmt.Fprintf(w, "removed %s\n", path)
	return nil
}
