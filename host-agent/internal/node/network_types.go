package node

import "context"

// NetworkAddress describes an address configured on this host, not a free IP pool.
type NetworkAddress struct {
	Family       string `json:"family"`
	Address      string `json:"address"`
	PrefixLength int    `json:"prefix_length"`
	Netmask      string `json:"netmask"`
	Network      string `json:"network"`
}

// DefaultRoute is a candidate route in the indicated table. Policy routing may
// select a different table; no single gateway is implicitly chosen for callers.
type DefaultRoute struct {
	Family  string `json:"family"`
	Gateway string `json:"gateway"`
	Metric  uint32 `json:"metric"`
	Table   uint32 `json:"table"`
	Weight  int    `json:"weight"`
}

type InterfaceDNS struct {
	Servers []string `json:"servers"`
	Source  string   `json:"source"`
	Error   string   `json:"error,omitempty"`
}

type SystemDNS struct {
	Servers       []string `json:"servers"`
	StubServers   []string `json:"stub_servers"`
	SearchDomains []string `json:"search_domains"`
	Source        string   `json:"source"`
	Error         string   `json:"error,omitempty"`
}

type NetworkInterface struct {
	Index         int              `json:"index"`
	Name          string           `json:"name"`
	MAC           string           `json:"mac"`
	MTU           int              `json:"mtu"`
	Up            bool             `json:"up"`
	Loopback      bool             `json:"loopback"`
	Addresses     []NetworkAddress `json:"addresses"`
	DefaultRoutes []DefaultRoute   `json:"default_routes"`
	DNS           InterfaceDNS     `json:"dns"`
	Error         string           `json:"error,omitempty"`
}

type NetworkInfo struct {
	Interfaces []NetworkInterface
	SystemDNS  SystemDNS
	Errors     []string
}

func collectNetwork(ctx context.Context) NetworkInfo {
	interfaces, errors := collectNetworkInterfaces(ctx)
	systemDNS, linkDNS := collectNetworkDNS(ctx, interfaces)
	for i := range interfaces {
		if dns, ok := linkDNS[interfaces[i].Index]; ok {
			interfaces[i].DNS = dns
		} else {
			interfaces[i].DNS = InterfaceDNS{Servers: []string{}, Source: "unavailable", Error: "per-interface DNS could not be determined"}
		}
	}
	return NetworkInfo{Interfaces: interfaces, SystemDNS: systemDNS, Errors: errors}
}
