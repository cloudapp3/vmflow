//go:build !windows

package statsstore

import "os"

func replaceStatsFile(source, target string) error {
	return os.Rename(source, target)
}

func syncStatsDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
