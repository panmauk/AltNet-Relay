package main

import (
	"time"

	"golang.org/x/sys/windows"
)

// waitForExit returns when process pid has exited, or after max.
func waitForExit(pid int, max time.Duration) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return // already gone
	}
	defer windows.CloseHandle(h)
	_, _ = windows.WaitForSingleObject(h, uint32(max/time.Millisecond))
}
