//go:build !linux

package monitor

import "os"

func deviceNumber(os.FileInfo) (uint64, bool) { return 0, false }
func supportsProcessAttribution() bool        { return false }
