//go:build !linux

package node

import (
	"context"
	"errors"
	"testing"
)

func TestIPProbeUnsupportedPlatform(t *testing.T) {
	result, err := probeIPv4(context.Background(), IPProbeRequest{Interface: "eth0", Address: "192.0.2.112"})
	var probeErr *ProbeError
	if !errors.As(err, &probeErr) || probeErr.Status != 501 || probeErr.Code != "unsupported_platform" {
		t.Fatalf("non-Linux must explicitly reject raw ARP probing: %+v %v", result, err)
	}
	if result.Status != IPProbeStatusUnknown || result.Available != nil || result.Warning != IPProbeWarning || len(result.Responders) != 0 {
		t.Fatalf("unsupported platform must not synthesize availability: %+v", result)
	}
}
