//go:build !linux

package node

import "context"

const dnsNonblockFlag = 0

func collectNetworkDNS(ctx context.Context, interfaces []NetworkInterface) (SystemDNS, map[int]InterfaceDNS) {
	return collectDNSFromSources(ctx, interfaces, readDNSFile, nil, "per-interface DNS discovery is unavailable on this operating system; Linux systemd-resolved was not queried")
}
