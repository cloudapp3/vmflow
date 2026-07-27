//go:build !windows

package certpath

import "os"

func replaceFile(source, target string) error {
	return os.Rename(source, target)
}

func syncCertificateDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
