//go:build !windows

package main

import (
	"syscall"
	"time"
)

// waitForExit returns when process pid has exited, or after max.
func waitForExit(pid int, max time.Duration) {
	end := time.Now().Add(max)
	for time.Now().Before(end) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
