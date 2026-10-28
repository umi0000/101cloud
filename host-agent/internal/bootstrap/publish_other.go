//go:build !linux && !darwin

package bootstrap

import "errors"

func publishDirectory(stage, destination string) error {
	return errors.New("atomic no-replace directory publication requires Linux or macOS")
}
