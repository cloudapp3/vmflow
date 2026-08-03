//go:build windows

package main

import "golang.org/x/sys/windows"

func currentProcessElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}
