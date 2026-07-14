//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package controlapi

import (
	"fmt"
	"os"
	"syscall"
)

func preserveConfigMetadata(temp *os.File, _ string, _ string, targetInfo os.FileInfo) error {
	targetStat, ok := targetInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("target file ownership is unavailable")
	}
	tempInfo, err := temp.Stat()
	if err != nil {
		return err
	}
	tempStat, ok := tempInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("temporary file ownership is unavailable")
	}
	if tempStat.Uid != targetStat.Uid || tempStat.Gid != targetStat.Gid {
		if err := temp.Chown(int(targetStat.Uid), int(targetStat.Gid)); err != nil {
			return err
		}
	}
	mode := targetInfo.Mode().Perm() | targetInfo.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
	return temp.Chmod(mode)
}

func replaceConfigFile(source, target string) error {
	return os.Rename(source, target)
}

func syncConfigDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
