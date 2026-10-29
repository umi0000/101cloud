//go:build !linux || (!amd64 && !arm64)

package gpu

func openBackend() (backend, error) { return nil, ErrUnsupported }
