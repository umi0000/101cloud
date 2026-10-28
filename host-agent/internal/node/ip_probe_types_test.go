package node

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func probeTestInterface(name, address string) probeInterface {
	iface := probeInterface{iface: net.Interface{
		Index:        2,
		Name:         name,
		HardwareAddr: net.HardwareAddr{2, 0, 0, 0, 0, 1},
		Flags:        net.FlagUp | net.FlagRunning | net.FlagBroadcast,
	}}
	if address != "" {
		iface.prefixes = []netip.Prefix{netip.MustParsePrefix(address)}
	}
	return iface
}

func TestValidateIPProbeNetwork(t *testing.T) {
	tests := []struct {
		name    string
		address string
		prefix  string
		modify  func(*probeInterface)
		code    string
		status  int
	}{
		{name: "direct host", address: "192.0.2.112", prefix: "192.0.2.10/24"},
		{name: "slash31 low peer", address: "192.0.2.10", prefix: "192.0.2.11/31"},
		{name: "slash31 high peer", address: "192.0.2.11", prefix: "192.0.2.10/31"},
		{name: "off link", address: "192.0.3.112", prefix: "192.0.2.10/24", code: "off_link_address", status: 400},
		{name: "network", address: "192.0.2.0", prefix: "192.0.2.10/24", code: "network_address", status: 400},
		{name: "broadcast", address: "192.0.2.255", prefix: "192.0.2.10/24", code: "broadcast_address", status: 400},
		{name: "slash32 off link", address: "192.0.2.11", prefix: "192.0.2.10/32", code: "off_link_address", status: 400},
		{name: "no IPv4 prefix", address: "192.0.2.112", code: "off_link_address", status: 400},
		{name: "unspecified", address: "0.0.0.0", prefix: "0.0.0.1/0", code: "invalid_address", status: 400},
		{name: "multicast", address: "224.0.0.1", prefix: "192.0.2.10/0", code: "invalid_address", status: 400},
		{name: "limited broadcast", address: "255.255.255.255", prefix: "192.0.2.10/0", code: "invalid_address", status: 400},
		{name: "IPv6", address: "2001:db8::1", code: "invalid_address", status: 400},
		{name: "mapped IPv6", address: "::ffff:192.0.2.112", code: "invalid_address", status: 400},
		{name: "prefix not host", address: "192.0.2.0/24", code: "invalid_address", status: 400},
		{name: "multiple addresses", address: "192.0.2.1,192.0.2.2", code: "invalid_address", status: 400},
		{name: "empty address", code: "invalid_address", status: 400},
		{name: "loopback interface", address: "192.0.2.112", prefix: "192.0.2.10/24", modify: func(iface *probeInterface) { iface.iface.Flags |= net.FlagLoopback }, code: "invalid_interface", status: 400},
		{name: "non Ethernet", address: "192.0.2.112", prefix: "192.0.2.10/24", modify: func(iface *probeInterface) { iface.iface.HardwareAddr = net.HardwareAddr{2, 0, 1} }, code: "invalid_interface", status: 400},
		{name: "zero hardware", address: "192.0.2.112", prefix: "192.0.2.10/24", modify: func(iface *probeInterface) { iface.iface.HardwareAddr = make(net.HardwareAddr, 6) }, code: "invalid_interface", status: 400},
		{name: "multicast hardware", address: "192.0.2.112", prefix: "192.0.2.10/24", modify: func(iface *probeInterface) { iface.iface.HardwareAddr[0] = 1 }, code: "invalid_interface", status: 400},
		{name: "administratively down", address: "192.0.2.112", prefix: "192.0.2.10/24", modify: func(iface *probeInterface) { iface.iface.Flags &^= net.FlagUp }, code: "interface_down", status: 503},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			iface := probeTestInterface("eth0", test.prefix)
			if test.modify != nil {
				test.modify(&iface)
			}
			network, result, err := validateIPProbe(IPProbeRequest{Interface: "eth0", Address: test.address}, []probeInterface{iface})
			if test.code == "" {
				if err != nil || network.target.String() != test.address {
					t.Fatalf("directly connected target: network=%+v err=%v", network, err)
				}
			} else {
				var probeErr *ProbeError
				if !errors.As(err, &probeErr) || probeErr.Code != test.code || probeErr.Status != test.status {
					t.Fatalf("got %v; want %s (%d)", err, test.code, test.status)
				}
			}
			if result.Status != IPProbeStatusUnknown || result.Available != nil || result.Responders == nil || result.Warning != IPProbeWarning {
				t.Fatalf("validation cannot infer availability: %+v", result)
			}
		})
	}
}

func TestValidateIPProbeMissingInterface(t *testing.T) {
	for _, name := range []string{"", "missing"} {
		_, result, err := validateIPProbe(IPProbeRequest{Interface: name, Address: "192.0.2.112"}, []probeInterface{probeTestInterface("eth0", "192.0.2.10/24")})
		var probeErr *ProbeError
		if !errors.As(err, &probeErr) || probeErr.Code != "invalid_interface" || result.Available != nil {
			t.Fatalf("missing interface: result=%+v err=%v", result, err)
		}
	}
}

func TestValidateIPProbeLocalAddressOnAnyInterface(t *testing.T) {
	for _, prefix := range []string{"192.0.2.112/24", "192.0.2.112/32", "198.51.100.112/24"} {
		t.Run(prefix, func(t *testing.T) {
			selected := probeTestInterface("eth0", "192.0.2.10/24")
			other := probeTestInterface("eth1", prefix)
			other.iface.Index = 3
			other.iface.HardwareAddr = net.HardwareAddr{2, 0, 0, 0, 0, 2}
			target := netip.MustParsePrefix(prefix).Addr().String()
			_, result, err := validateIPProbe(IPProbeRequest{Interface: "eth0", Address: target}, []probeInterface{selected, other})
			if err != nil || result.Status != IPProbeStatusOccupied || result.Available == nil || *result.Available || len(result.Responders) != 1 {
				t.Fatalf("local ownership: result=%+v err=%v", result, err)
			}
			if result.Responders[0].MAC != other.iface.HardwareAddr.String() || result.Responders[0].Source != "local_address" {
				t.Fatalf("local responder must identify owning interface: %+v", result.Responders)
			}
		})
	}
}

func TestValidateIPProbeOverlappingPrefix(t *testing.T) {
	iface := probeTestInterface("eth0", "192.0.2.10/24")
	iface.prefixes = append(iface.prefixes, netip.MustParsePrefix("192.0.3.10/23"))
	_, result, err := validateIPProbe(IPProbeRequest{Interface: "eth0", Address: "192.0.2.255"}, []probeInterface{iface})
	if err != nil || result.Available != nil {
		t.Fatalf("host address in a second direct prefix should be probeable: result=%+v err=%v", result, err)
	}
}

func TestIPProbeObservationDecisions(t *testing.T) {
	request := IPProbeRequest{Interface: "eth0", Address: "192.0.2.112"}
	clear := finishIPProbe(newIPProbeResult(request))
	if clear.Status != IPProbeStatusNoConflictDetected || clear.Available == nil || !*clear.Available || clear.Warning != IPProbeWarning {
		t.Fatalf("successful silence decision: %+v", clear)
	}
	occupied := newIPProbeResult(request)
	occupied.Responders = append(occupied.Responders, IPResponder{MAC: "02:00:00:00:00:02", Source: "arp_reply"})
	occupied = finishIPProbe(occupied)
	if occupied.Status != IPProbeStatusOccupied || occupied.Available == nil || *occupied.Available {
		t.Fatalf("positive conflict decision: %+v", occupied)
	}
	failed, err := probeFailure(clear, 503, "socket_error", "socket failed")
	if err == nil || failed.Status != IPProbeStatusUnknown || failed.Available != nil {
		t.Fatalf("failed observation must erase positive availability: %+v %v", failed, err)
	}
}

func TestIPProbeCanceledBeforeNetworkAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := probeIPv4(ctx, IPProbeRequest{Interface: "does-not-exist", Address: "192.0.2.112"})
	var probeErr *ProbeError
	if !errors.As(err, &probeErr) || probeErr.Code != "probe_canceled" || result.Status != IPProbeStatusUnknown || result.Available != nil {
		t.Fatalf("cancellation must precede interface/socket access: %+v %v", result, err)
	}
}

func TestIPProbeContextFailure(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{context.Canceled, 503, "probe_canceled"},
		{context.DeadlineExceeded, 504, "probe_timeout"},
	} {
		result, err := probeContextFailure(newIPProbeResult(IPProbeRequest{}), test.err)
		var probeErr *ProbeError
		if !errors.As(err, &probeErr) || probeErr.Status != test.status || probeErr.Code != test.code || result.Available != nil {
			t.Fatalf("context failure: %+v %v", result, err)
		}
	}
}
