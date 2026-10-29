package node

import (
	"context"
	"errors"
	"net"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestNetworkAddressPrefixes(t *testing.T) {
	for _, test := range []struct {
		input string
		want  NetworkAddress
	}{
		{"192.0.2.129/23", NetworkAddress{Family: "ipv4", Address: "192.0.2.129", PrefixLength: 23, Netmask: "255.255.254.0", Network: "192.0.2.0/23"}},
		{"198.51.100.7/32", NetworkAddress{Family: "ipv4", Address: "198.51.100.7", PrefixLength: 32, Netmask: "255.255.255.255", Network: "198.51.100.7/32"}},
		{"203.0.113.7/0", NetworkAddress{Family: "ipv4", Address: "203.0.113.7", PrefixLength: 0, Netmask: "0.0.0.0", Network: "0.0.0.0/0"}},
		{"2001:db8:abcd:1234::123/64", NetworkAddress{Family: "ipv6", Address: "2001:db8:abcd:1234::123", PrefixLength: 64, Netmask: "ffff:ffff:ffff:ffff::", Network: "2001:db8:abcd:1234::/64"}},
		{"fe80::1234/128", NetworkAddress{Family: "ipv6", Address: "fe80::1234", PrefixLength: 128, Netmask: "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", Network: "fe80::1234/128"}},
		{"2001:db8::1/0", NetworkAddress{Family: "ipv6", Address: "2001:db8::1", PrefixLength: 0, Netmask: "::", Network: "::/0"}},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := networkAddress(test.input)
			if err != nil || got != test.want {
				t.Fatalf("networkAddress(%q) = %#v, %v; want %#v", test.input, got, err, test.want)
			}
		})
	}
	for _, input := range []string{"192.0.2.1", "192.0.2.1/33", "2001:db8::1/129", "fe80::1%eth0/64", "bad/24", ""} {
		if _, err := networkAddress(input); err == nil {
			t.Errorf("accepted invalid interface prefix %q", input)
		}
	}
}

func TestIPDefaultRoutesMultipleTablesAndECMP(t *testing.T) {
	data := []byte(`[
		{"dst":"192.0.2.0/24","dev":"eth0","scope":"link"},
		{"dst":"default","gateway":"192.0.2.1","dev":"eth0","metric":100},
		{"dst":"0.0.0.0/0","table":1001,"metric":7,"nexthops":[
			{"gateway":"198.51.100.1","dev":"vlan100","weight":2},
			{"gateway":"203.0.113.1","dev":"bridge0","weight":256}
		]},
		{"dst":"default","dev":"ppp0","table":"4294967295","metric":4294967295},
		{"type":"blackhole","dst":"default","table":102},
		{"dst":"default","dev":"veth0","table":"default"}
	]`)
	got, errs := parseIPDefaultRoutes(data, "ipv4")
	want := []interfaceDefaultRoute{
		{device: "eth0", route: DefaultRoute{Family: "ipv4", Gateway: "192.0.2.1", Metric: 100, Table: 254, Weight: 1}},
		{device: "vlan100", route: DefaultRoute{Family: "ipv4", Gateway: "198.51.100.1", Metric: 7, Table: 1001, Weight: 2}},
		{device: "bridge0", route: DefaultRoute{Family: "ipv4", Gateway: "203.0.113.1", Metric: 7, Table: 1001, Weight: 256}},
		{device: "ppp0", route: DefaultRoute{Family: "ipv4", Gateway: "", Metric: 4294967295, Table: 4294967295, Weight: 1}},
		{device: "veth0", route: DefaultRoute{Family: "ipv4", Gateway: "", Table: 253, Weight: 1}},
	}
	if len(errs) != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %#v, errors = %v; want %#v", got, errs, want)
	}
}

func TestIPDefaultRoutesIPv6AndCrossFamilyGateway(t *testing.T) {
	got, errs := parseIPDefaultRoutes([]byte(`[
		{"dst":"default","dev":"eth0","gateway":"fe80::1","table":"main","metric":1024},
		{"dst":"::/0","table":200,"metric":64,"nexthops":[
			{"dev":"eth1","gateway":"2001:db8:0:0::1","weight":3},
			{"dev":"eth2","weight":1}
		]}
	]`), "ipv6")
	want := []interfaceDefaultRoute{
		{device: "eth0", route: DefaultRoute{Family: "ipv6", Gateway: "fe80::1", Table: 254, Metric: 1024, Weight: 1}},
		{device: "eth1", route: DefaultRoute{Family: "ipv6", Gateway: "2001:db8::1", Table: 200, Metric: 64, Weight: 3}},
		{device: "eth2", route: DefaultRoute{Family: "ipv6", Table: 200, Metric: 64, Weight: 1}},
	}
	if len(errs) != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("IPv6 routes = %#v, errors = %v; want %#v", got, errs, want)
	}
	got, errs = parseIPDefaultRoutes([]byte(`[{"dst":"default","dev":"eth0","via":{"family":"inet6","host":"2001:db8::1"}}]`), "ipv4")
	if len(errs) != 0 || len(got) != 1 || got[0].route.Family != "ipv4" || got[0].route.Gateway != "2001:db8::1" {
		t.Fatalf("cross-family gateway was lost: %#v, %v", got, errs)
	}
}

func TestIPDefaultRouteInvalidRowsDoNotInventGateways(t *testing.T) {
	for _, row := range []string{
		`{"dst":"default","gateway":"192.0.2.1"}`,
		`{"dst":"default","dev":"eth0","gateway":"not-an-ip"}`,
		`{"dst":"default","dev":"eth0","gateway":"2001:db8::1"}`,
		`{"dst":"default","dev":"eth0","gateway":"192.0.2.1","metric":-1}`,
		`{"dst":"default","dev":"eth0","table":4294967296}`,
		`{"dst":"default","dev":"eth0","metric":1.5}`,
		`{"dst":"default","dev":"eth0","metric":null}`,
		`{"dst":"default","dev":"eth0","table":"private-custom-table"}`,
		`{"dst":"default","dev":"eth0","nhid":100}`,
		`{"dst":"default","nhid":101}`,
		`{"dst":"default","dev":"eth0","via":null}`,
		`{"dst":"default","dev":"eth0","gateway":null}`,
		`{"dst":"default","dev":"eth0","gateway":""}`,
		`{"dst":"default","dev":"eth0","gateway":1}`,
		`{"dst":"default","dev":"eth0","via":{"family":"inet6","host":"192.0.2.1"}}`,
		`{"dst":"default","dev":"eth0","gateway":"192.0.2.1","via":{"family":"inet","host":"192.0.2.2"}}`,
		`{"dst":"default","nexthops":[{"dev":"eth0","weight":0}]}`,
		`{"dst":"default","nexthops":[{"dev":"eth0","weight":257}]}`,
		`{"dst":"default","nexthops":[{"dev":"eth0","weight":-1}]}`,
		`{"dst":"default","nexthops":[{"dev":"eth0","weight":null}]}`,
	} {
		t.Run(row, func(t *testing.T) {
			data := []byte("[" + row + `,{"dst":"default","dev":"eth1","gateway":"198.51.100.1"}]`)
			got, errs := parseIPDefaultRoutes(data, "ipv4")
			if len(errs) == 0 || len(got) != 1 || got[0].device != "eth1" || got[0].route.Gateway != "198.51.100.1" {
				t.Fatalf("invalid row became a successful route or hid a valid sibling: %#v, %v", got, errs)
			}
		})
	}
	got, errs := parseIPDefaultRoutes([]byte(`[{"dst":"default","metric":5,"table":101,"nexthops":[{"dev":"eth0","gateway":"bad"},{"dev":"eth1","gateway":"192.0.2.1","weight":2}]}]`), "ipv4")
	if len(errs) != 1 || len(got) != 1 || got[0].device != "eth1" || got[0].route.Table != 101 || got[0].route.Weight != 2 {
		t.Fatalf("invalid ECMP hop hid valid sibling: %#v, %v", got, errs)
	}
}

func TestIPDefaultRouteJSONBoundaries(t *testing.T) {
	for _, data := range []string{"", "null", "{}", "[", "[] []", "[] garbage", `[{"dst":"default","dev":7}]`, strings.Repeat(" ", maxNetworkRouteOutputBytes+1)} {
		got, errs := parseIPDefaultRoutes([]byte(data), "ipv4")
		if len(got) != 0 || len(errs) == 0 {
			t.Errorf("accepted invalid JSON of length %d: %#v, %v", len(data), got, errs)
		}
	}
	got, errs := parseIPDefaultRoutes([]byte("[]"), "ipv4")
	if got == nil || len(got) != 0 || len(errs) != 0 {
		t.Fatalf("empty route list is not a successful empty result: %#v, %v", got, errs)
	}
	if _, errs := parseIPDefaultRoutes([]byte("[]"), "other"); len(errs) == 0 {
		t.Fatal("accepted unknown route family")
	}
}

func TestInterfaceRouteFailuresPreserveInventoryAndOtherFamily(t *testing.T) {
	interfaces := []NetworkInterface{{Index: 2, Name: "eth0", MAC: "02:00:00:00:00:01", MTU: 1500, Up: true,
		Addresses:     []NetworkAddress{{Family: "ipv4", Address: "192.0.2.10", PrefixLength: 24, Network: "192.0.2.0/24"}},
		DefaultRoutes: []DefaultRoute{},
	}}
	errs := collectInterfaceDefaultRoutes(context.Background(), interfaces, func(_ context.Context, family string) ([]byte, error) {
		if family == "ipv4" {
			return nil, errors.New("route permission denied")
		}
		return []byte(`[{"dst":"default","dev":"eth0","gateway":"fe80::1"},{"dst":"default","dev":"removed0","gateway":"fe80::2"}]`), nil
	})
	if len(errs) != 2 || !strings.Contains(errs[0], "permission denied") || !strings.Contains(errs[1], "removed0") {
		t.Fatalf("missing route failures: %v", errs)
	}
	if interfaces[0].Index != 2 || interfaces[0].MAC != "02:00:00:00:00:01" || !interfaces[0].Up || len(interfaces[0].Addresses) != 1 {
		t.Fatalf("route failure discarded interface inventory: %#v", interfaces)
	}
	if len(interfaces[0].DefaultRoutes) != 1 || interfaces[0].DefaultRoutes[0].Gateway != "fe80::1" {
		t.Fatalf("route failure fabricated IPv4 success or lost IPv6: %#v", interfaces[0].DefaultRoutes)
	}
}

func TestInterfaceDefaultRouteStableOrdering(t *testing.T) {
	interfaces := []NetworkInterface{{Name: "eth0", DefaultRoutes: []DefaultRoute{}}}
	errs := collectInterfaceDefaultRoutes(context.Background(), interfaces, func(_ context.Context, family string) ([]byte, error) {
		if family == "ipv6" {
			return []byte(`[{"dst":"default","dev":"eth0","gateway":"fe80::1"}]`), nil
		}
		return []byte(`[
			{"dst":"default","dev":"eth0","table":500,"metric":1,"gateway":"192.0.2.1"},
			{"dst":"default","dev":"eth0","table":100,"metric":20,"gateway":"192.0.2.1"},
			{"dst":"default","table":100,"metric":10,"nexthops":[{"dev":"eth0","gateway":"192.0.2.2","weight":2},{"dev":"eth0","gateway":"192.0.2.1","weight":3},{"dev":"eth0","gateway":"192.0.2.1","weight":1}]}
		]`), nil
	})
	want := []DefaultRoute{
		{Family: "ipv4", Table: 100, Metric: 10, Gateway: "192.0.2.1", Weight: 1},
		{Family: "ipv4", Table: 100, Metric: 10, Gateway: "192.0.2.1", Weight: 3},
		{Family: "ipv4", Table: 100, Metric: 10, Gateway: "192.0.2.2", Weight: 2},
		{Family: "ipv4", Table: 100, Metric: 20, Gateway: "192.0.2.1", Weight: 1},
		{Family: "ipv4", Table: 500, Metric: 1, Gateway: "192.0.2.1", Weight: 1},
		{Family: "ipv6", Table: 254, Gateway: "fe80::1", Weight: 1},
	}
	if len(errs) != 0 || !reflect.DeepEqual(interfaces[0].DefaultRoutes, want) {
		t.Fatalf("unstable route order: %#v; %v", interfaces[0].DefaultRoutes, errs)
	}
}

func TestIPRouteCommandCancellationAndUnavailableBinary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if data, err := queryIPRoutes(ctx, "ipv4"); !errors.Is(err, context.Canceled) || data != nil {
		t.Fatalf("canceled route command = %q, %v", data, err)
	}
	t.Setenv("PATH", t.TempDir())
	if data, err := queryIPRoutes(context.Background(), "ipv6"); err == nil || data != nil {
		t.Fatalf("missing ip command became success: %q, %v", data, err)
	}
	if _, err := queryIPRoutes(context.Background(), "other"); err == nil {
		t.Fatal("accepted unknown command route family")
	}
}

func TestIPRouteOutputCombinedBound(t *testing.T) {
	output := &networkRouteOutput{}
	stdout := networkRouteOutputWriter{output: output}
	stderr := networkRouteOutputWriter{output: output, stderr: true}
	if n, err := stdout.Write([]byte(strings.Repeat("x", maxNetworkRouteOutputBytes-3))); err != nil || n != maxNetworkRouteOutputBytes-3 {
		t.Fatalf("valid bounded output rejected: %d, %v", n, err)
	}
	if n, err := stderr.Write([]byte("err")); err != nil || n != 3 {
		t.Fatalf("exact combined output limit rejected: %d, %v", n, err)
	}
	if n, err := stdout.Write([]byte("overflow")); err == nil || n != 0 || !output.exceeded {
		t.Fatalf("combined stdout/stderr limit not enforced: %d, %v", n, err)
	}
	if output.stdout.Len()+output.stderr.Len() != maxNetworkRouteOutputBytes {
		t.Fatal("output exceeded the combined byte bound")
	}
}

func TestCanceledNetworkInventoryRetainsRealInterfaces(t *testing.T) {
	actual, err := net.Interfaces()
	if err != nil {
		t.Fatalf("read actual interface inventory: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, collectionErrors := collectNetworkInterfaces(ctx)
	if len(collectionErrors) == 0 {
		t.Fatal("cancellation or unsupported route platform must remain explicit")
	}
	if len(got) != len(actual) {
		t.Fatalf("cancellation discarded interfaces: got %d, want %d", len(got), len(actual))
	}
	byIndex := make(map[int]net.Interface, len(actual))
	for _, iface := range actual {
		byIndex[iface.Index] = iface
	}
	for index, iface := range got {
		original, ok := byIndex[iface.Index]
		if !ok || iface.Name != original.Name || iface.MTU != original.MTU || iface.MAC != original.HardwareAddr.String() || iface.Up != (original.Flags&net.FlagUp != 0) || iface.Loopback != (original.Flags&net.FlagLoopback != 0) {
			t.Errorf("base interface data changed on route failure: %#v, original %#v", iface, original)
		}
		if iface.Addresses == nil || iface.DefaultRoutes == nil || iface.DNS.Servers == nil {
			t.Errorf("inventory arrays must not be null: %#v", iface)
		}
		if len(iface.DefaultRoutes) != 0 {
			t.Errorf("canceled collection invented default routes: %#v", iface.DefaultRoutes)
		}
		if index > 0 && got[index-1].Index > iface.Index {
			t.Error("interfaces are not sorted by index")
		}
		for _, address := range iface.Addresses {
			ip, prefix, parseErr := net.ParseCIDR(address.Address + "/" + strconv.Itoa(address.PrefixLength))
			if parseErr != nil || ip == nil || prefix.String() != address.Network || net.IP(prefix.Mask).String() != address.Netmask {
				t.Errorf("real interface prefix lost network or mask: %#v", address)
			}
		}
	}
	if runtime.GOOS != "linux" && !strings.Contains(strings.Join(collectionErrors, ";"), "only on Linux") {
		t.Fatalf("non-Linux host did not report unsupported route collection: %v", collectionErrors)
	}
}
