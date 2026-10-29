//go:build !linux

package node

import (
	"context"
	"strings"
	"testing"
)

func TestNetworkDNSOtherPlatformDoesNotClaimResolvedDiscovery(t *testing.T) {
	system, links := collectNetworkDNS(context.Background(), []NetworkInterface{{Index: 2, Name: "test0"}})
	if strings.HasPrefix(system.Source, "systemd-resolved") || !strings.Contains(system.Error, "Linux systemd-resolved was not queried") {
		t.Fatalf("non-Linux claimed resolved discovery: %+v", system)
	}
	link := links[2]
	if link.Source != "unavailable" || len(link.Servers) != 0 || !strings.Contains(link.Error, "operating system") {
		t.Fatalf("non-Linux fabricated per-interface DNS: %+v", link)
	}
}
