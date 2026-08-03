//go:build linux || darwin

package main

import "os"

func currentProcessElevated() bool {
	return os.Geteuid() == 0
}
