//go:build linux

package monitor

import (
	"os"
	"syscall"
)

func deviceNumber(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Rdev), true
}

func supportsProcessAttribution() bool { return true }
