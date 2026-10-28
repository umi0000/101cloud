//go:build !linux

package node

import "context"

func probeIPv4(ctx context.Context, req IPProbeRequest) (IPProbeResult, error) {
	result := newIPProbeResult(req)
	if err := ctx.Err(); err != nil {
		return probeContextFailure(result, err)
	}
	return probeFailure(result, 501, "unsupported_platform", "ARP conflict probing is supported only on Linux")
}
