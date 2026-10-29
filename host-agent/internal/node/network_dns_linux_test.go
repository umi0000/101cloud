//go:build linux

package node

import (
	"context"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

func resolvedTestServer(index int32, address string) resolvedDNSServer {
	ip := netip.MustParseAddr(address)
	family := int32(unix.AF_INET6)
	if ip.Is4() {
		family = unix.AF_INET
	}
	return resolvedDNSServer{Index: index, Family: family, Address: ip.AsSlice()}
}

func TestResolvedDNSKeepsGlobalAndPerLinkScopes(t *testing.T) {
	interfaces := []NetworkInterface{{Index: 2, Name: "eth0"}, {Index: 7, Name: "eth1"}, {Index: 9, Name: "eth2"}}
	servers := []resolvedDNSServer{
		resolvedTestServer(0, "192.0.2.53"),
		resolvedTestServer(2, "198.51.100.53"),
		resolvedTestServer(7, "203.0.113.53"),
		resolvedTestServer(2, "198.51.100.53"),
		resolvedTestServer(100, "192.0.2.100"),
	}
	domains := []resolvedDNSDomain{
		{Index: 0, Domain: "global.test"},
		{Index: 2, Domain: "corp.test"},
		{Index: 7, Domain: "private.test", RouteOnly: true},
		{Index: 7, Domain: ".", RouteOnly: true},
		{Index: 7, Domain: "corp.test"},
	}
	system, links := decodeResolvedDNS(interfaces, servers, domains)
	if system.Source != "systemd-resolved" || !reflect.DeepEqual(system.Servers, []string{"192.0.2.53"}) || !reflect.DeepEqual(system.SearchDomains, []string{"global.test"}) || system.Error != "" {
		t.Fatalf("global scope or domains lost: %+v", system)
	}
	for index, want := range map[int][]string{2: {"198.51.100.53"}, 7: {"203.0.113.53"}} {
		if links[index].Source != "systemd-resolved" || links[index].Error != "" || !reflect.DeepEqual(links[index].Servers, want) {
			t.Fatalf("wrong DNS association for %d: %+v", index, links[index])
		}
	}
	if links[9].Source != "unavailable" || links[9].Error == "" || len(links[9].Servers) != 0 || len(links) != 3 {
		t.Fatalf("missing or vanished link acquired invented DNS: %+v", links)
	}
}

func TestResolvedDNSIPv6ZonesAndLoopback(t *testing.T) {
	interfaces := []NetworkInterface{{Index: 2, Name: "enp1s0"}, {Index: 7, Name: "eth1"}}
	servers := []resolvedDNSServer{
		resolvedTestServer(0, "2001:db8::53"),
		resolvedTestServer(0, "127.0.0.53"),
		resolvedTestServer(0, "::1"),
		resolvedTestServer(2, "fe80::53"),
		resolvedTestServer(2, "2001:db8:2::53"),
		resolvedTestServer(7, "127.0.0.1"),
	}
	system, links := decodeResolvedDNS(interfaces, servers, nil)
	if !reflect.DeepEqual(system.Servers, []string{"2001:db8::53"}) || !reflect.DeepEqual(system.StubServers, []string{"127.0.0.53", "::1", "127.0.0.1"}) || !reflect.DeepEqual(links[2].Servers, []string{"fe80::53%enp1s0", "2001:db8:2::53"}) {
		t.Fatalf("IPv6 scope/stub filtering failed: %+v %+v", system, links)
	}
	if links[7].Source != "unavailable" || len(links[7].Servers) != 0 || !strings.Contains(links[7].Error, "loopback") {
		t.Fatalf("host-local stub advertised as upstream: %+v", links[7])
	}
	_, err := decodeResolvedAddress(resolvedTestServer(0, "fe80::53"), "")
	if err == nil || !strings.Contains(err.Error(), "zone") {
		t.Fatalf("unscoped link-local DNS accepted: %v", err)
	}
}

func TestResolvedDNSMalformedEntriesRemainVisible(t *testing.T) {
	interfaces := []NetworkInterface{{Index: 2, Name: "eth0"}}
	servers := []resolvedDNSServer{
		{Index: 0, Family: 123, Address: []byte{1}},
		{Index: 2, Family: unix.AF_INET, Address: []byte{192, 0}},
		{Index: 2, Family: unix.AF_INET6, Address: []byte{0}},
		resolvedTestServer(-1, "192.0.2.53"),
		resolvedTestServer(2, "0.0.0.0"),
		resolvedTestServer(2, "ff02::1"),
		resolvedTestServer(2, "192.0.2.53"),
	}
	system, links := decodeResolvedDNS(interfaces, servers, nil)
	if !strings.Contains(system.Error, "unsupported DNS address family") || !strings.Contains(system.Error, "interface -1") || len(system.Servers) != 0 {
		t.Fatalf("malformed global data not reported: %+v", system)
	}
	if links[2].Source != "systemd-resolved" || !reflect.DeepEqual(links[2].Servers, []string{"192.0.2.53"}) || !strings.Contains(links[2].Error, "IPv4 DNS length") || !strings.Contains(links[2].Error, "IPv6 DNS length") || !strings.Contains(links[2].Error, "invalid DNS address") {
		t.Fatalf("partial valid data hid malformed DNS: %+v", links[2])
	}
}

func TestResolvedDBusTupleDecoding(t *testing.T) {
	// Real DBus tuple representations, not a flattened manager-wide server list.
	value := []interface{}{
		[]interface{}{int32(0), int32(unix.AF_INET), []byte{192, 0, 2, 53}},
		[]interface{}{int32(2), int32(unix.AF_INET6), netip.MustParseAddr("fe80::53").AsSlice()},
	}
	var servers []resolvedDNSServer
	if err := dbus.Store([]interface{}{value}, &servers); err != nil {
		t.Fatal(err)
	}
	var domains []resolvedDNSDomain
	if err := dbus.Store([]interface{}{[]interface{}{[]interface{}{int32(2), "corp.test", false}}}, &domains); err != nil {
		t.Fatal(err)
	}
	system, links := decodeResolvedDNS([]NetworkInterface{{Index: 2, Name: "eth0"}}, servers, domains)
	if !reflect.DeepEqual(system.Servers, []string{"192.0.2.53"}) || !reflect.DeepEqual(links[2].Servers, []string{"fe80::53%eth0"}) || len(system.SearchDomains) != 0 {
		t.Fatalf("DBus tuples decoded incorrectly: %+v %+v", system, links)
	}
	if got := dbus.SignatureOf([]resolvedDNSServer{}).String(); got != "a(iiay)" {
		t.Fatalf("wrong DNS tuple signature: %s", got)
	}
	if got := dbus.SignatureOf([]resolvedDNSDomain{}).String(); got != "a(isb)" {
		t.Fatalf("wrong Domains tuple signature: %s", got)
	}
}

func TestResolvedCanceledContextDoesNotConnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// An untrusted environment must never select a different bus or transport.
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "tcp:host=192.0.2.1,port=1")
	_, _, err := readResolvedDNS(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "canceled") || !strings.Contains(err.Error(), resolvedSystemBusSocket) {
		t.Fatalf("canceled fixed-system-bus call: %v", err)
	}
}
