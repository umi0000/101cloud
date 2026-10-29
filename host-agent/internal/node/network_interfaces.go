package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxNetworkRouteOutputBytes = 1 << 20

// collectNetworkInterfaces inventories all interface kinds, including interfaces
// that are down or have no addresses. Route failures never discard this inventory.
func collectNetworkInterfaces(ctx context.Context) ([]NetworkInterface, []string) {
	result := make([]NetworkInterface, 0)
	collectionErrors := make([]string, 0)
	interfaces, err := net.Interfaces()
	if err != nil {
		return result, append(collectionErrors, fmt.Sprintf("list network interfaces: %v", err))
	}
	for _, iface := range interfaces {
		item := NetworkInterface{
			Index: iface.Index, Name: iface.Name, MAC: iface.HardwareAddr.String(), MTU: iface.MTU,
			Up: iface.Flags&net.FlagUp != 0, Loopback: iface.Flags&net.FlagLoopback != 0,
			Addresses: []NetworkAddress{}, DefaultRoutes: []DefaultRoute{}, DNS: InterfaceDNS{Servers: []string{}},
		}
		addresses, addrErr := iface.Addrs()
		if addrErr != nil {
			item.Error = fmt.Sprintf("list interface addresses: %v", addrErr)
			collectionErrors = append(collectionErrors, fmt.Sprintf("interface %s: %s", iface.Name, item.Error))
		}
		for _, address := range addresses {
			parsed, parseErr := networkAddress(address.String())
			if parseErr != nil {
				message := fmt.Sprintf("parse interface address %q: %v", address.String(), parseErr)
				if item.Error != "" {
					item.Error += "; "
				}
				item.Error += message
				collectionErrors = append(collectionErrors, fmt.Sprintf("interface %s: %s", iface.Name, message))
				continue
			}
			item.Addresses = append(item.Addresses, parsed)
		}
		sort.Slice(item.Addresses, func(i, j int) bool {
			a, b := item.Addresses[i], item.Addresses[j]
			if a.Family != b.Family {
				return a.Family < b.Family
			}
			if a.Address != b.Address {
				return a.Address < b.Address
			}
			return a.PrefixLength < b.PrefixLength
		})
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Index != result[j].Index {
			return result[i].Index < result[j].Index
		}
		return result[i].Name < result[j].Name
	})
	if runtime.GOOS != "linux" {
		return result, append(collectionErrors, "default route collection is supported only on Linux")
	}
	collectionErrors = append(collectionErrors, collectInterfaceDefaultRoutes(ctx, result, queryIPRoutes)...)
	return result, collectionErrors
}

func networkAddress(value string) (NetworkAddress, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return NetworkAddress{}, err
	}
	family, bits := "ipv6", 128
	if prefix.Addr().Is4() {
		family, bits = "ipv4", 32
	}
	mask := net.CIDRMask(prefix.Bits(), bits)
	return NetworkAddress{
		Family: family, Address: prefix.Addr().String(), PrefixLength: prefix.Bits(),
		Netmask: net.IP(mask).String(), Network: prefix.Masked().String(),
	}, nil
}

type networkRouteQuery func(context.Context, string) ([]byte, error)

func collectInterfaceDefaultRoutes(ctx context.Context, interfaces []NetworkInterface, query networkRouteQuery) []string {
	collectionErrors := make([]string, 0)
	byName := make(map[string]int, len(interfaces))
	for i := range interfaces {
		byName[interfaces[i].Name] = i
	}
	for _, family := range []string{"ipv4", "ipv6"} {
		data, err := query(ctx, family)
		if err != nil {
			collectionErrors = append(collectionErrors, fmt.Sprintf("%s default routes: %v", family, err))
			continue
		}
		routes, routeErrors := parseIPDefaultRoutes(data, family)
		collectionErrors = append(collectionErrors, routeErrors...)
		for _, route := range routes {
			index, ok := byName[route.device]
			if !ok {
				collectionErrors = append(collectionErrors, fmt.Sprintf("%s default route references unavailable interface %q", family, route.device))
				continue
			}
			interfaces[index].DefaultRoutes = append(interfaces[index].DefaultRoutes, route.route)
		}
	}
	for i := range interfaces {
		sort.SliceStable(interfaces[i].DefaultRoutes, func(a, b int) bool {
			x, y := interfaces[i].DefaultRoutes[a], interfaces[i].DefaultRoutes[b]
			if x.Family != y.Family {
				return x.Family < y.Family
			}
			if x.Table != y.Table {
				return x.Table < y.Table
			}
			if x.Metric != y.Metric {
				return x.Metric < y.Metric
			}
			if x.Gateway != y.Gateway {
				return x.Gateway < y.Gateway
			}
			return x.Weight < y.Weight
		})
	}
	return collectionErrors
}

// ip's numeric output avoids depending on host-specific rt_tables names. Its
// JSON omits the main table, zero metric and weight-one nexthop defaults.
func queryIPRoutes(ctx context.Context, family string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	flag := "-4"
	if family == "ipv6" {
		flag = "-6"
	} else if family != "ipv4" {
		return nil, fmt.Errorf("unsupported route family %q", family)
	}
	// Leave room for WaitDelay within the two-second command budget, including
	// descendants that inherited stdout/stderr after the immediate child exits.
	commandContext, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(commandContext, "ip", "-j", "-N", flag, "route", "show", "table", "all")
	command.WaitDelay = 100 * time.Millisecond
	output := &networkRouteOutput{}
	command.Stdout = networkRouteOutputWriter{output: output}
	command.Stderr = networkRouteOutputWriter{output: output, stderr: true}
	err := command.Run()
	if commandContext.Err() != nil {
		return nil, commandContext.Err()
	}
	if output.exceeded {
		return nil, fmt.Errorf("ip route output exceeds %d bytes", maxNetworkRouteOutputBytes)
	}
	if err != nil {
		message := strings.TrimSpace(output.stderr.String())
		if message != "" {
			return nil, fmt.Errorf("ip route: %w: %s", err, message)
		}
		return nil, fmt.Errorf("ip route: %w", err)
	}
	return output.stdout.Bytes(), nil
}

type networkRouteOutput struct {
	mu       sync.Mutex
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	exceeded bool
}

type networkRouteOutputWriter struct {
	output *networkRouteOutput
	stderr bool
}

func (writer networkRouteOutputWriter) Write(data []byte) (int, error) {
	output := writer.output
	output.mu.Lock()
	defer output.mu.Unlock()
	remaining := maxNetworkRouteOutputBytes - output.stdout.Len() - output.stderr.Len()
	buffer := &output.stdout
	if writer.stderr {
		buffer = &output.stderr
	}
	if len(data) > remaining {
		n, _ := buffer.Write(data[:remaining])
		output.exceeded = true
		return n, errors.New("ip route output exceeds size limit")
	}
	return buffer.Write(data)
}

type interfaceDefaultRoute struct {
	device string
	route  DefaultRoute
}

type ipJSONRoute struct {
	Type     string          `json:"type"`
	Dst      string          `json:"dst"`
	Dev      string          `json:"dev"`
	Gateway  json.RawMessage `json:"gateway"`
	Via      json.RawMessage `json:"via"`
	Metric   json.RawMessage `json:"metric"`
	Table    json.RawMessage `json:"table"`
	NHID     json.RawMessage `json:"nhid"`
	Nexthops []ipJSONNexthop `json:"nexthops"`
}

type ipJSONNexthop struct {
	Dev     string          `json:"dev"`
	Gateway json.RawMessage `json:"gateway"`
	Via     json.RawMessage `json:"via"`
	Weight  json.RawMessage `json:"weight"`
}

func parseIPDefaultRoutes(data []byte, family string) ([]interfaceDefaultRoute, []string) {
	result := make([]interfaceDefaultRoute, 0)
	parseErrors := make([]string, 0)
	if family != "ipv4" && family != "ipv6" {
		return result, append(parseErrors, fmt.Sprintf("unsupported route family %q", family))
	}
	if len(data) > maxNetworkRouteOutputBytes {
		return result, append(parseErrors, "ip route JSON exceeds size limit")
	}
	var records []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&records); err != nil || records == nil {
		return result, append(parseErrors, fmt.Sprintf("%s default routes: invalid ip route JSON array", family))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return result, append(parseErrors, fmt.Sprintf("%s default routes: trailing ip route JSON data", family))
	}
	for index, record := range records {
		var entry ipJSONRoute
		if err := json.Unmarshal(record, &entry); err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("%s route %d: invalid record: %v", family, index, err))
			continue
		}
		isDefault := entry.Dst == "default" || (family == "ipv4" && entry.Dst == "0.0.0.0/0") || (family == "ipv6" && entry.Dst == "::/0")
		if !isDefault {
			continue
		}
		// These are policy dispositions, not routes through an interface.
		if entry.Type != "" && entry.Type != "unicast" {
			continue
		}
		metric, err := routeUint32(entry.Metric, 0, false)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("%s route %d: invalid metric: %v", family, index, err))
			continue
		}
		table, err := routeUint32(entry.Table, 254, true)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("%s route %d: invalid table: %v", family, index, err))
			continue
		}
		if len(entry.NHID) != 0 && len(entry.Nexthops) == 0 && len(entry.Gateway) == 0 && len(entry.Via) == 0 {
			parseErrors = append(parseErrors, fmt.Sprintf("%s route %d: unresolved nexthop ID", family, index))
			continue
		}
		hops := entry.Nexthops
		if len(hops) == 0 {
			hops = []ipJSONNexthop{{Dev: entry.Dev, Gateway: entry.Gateway, Via: entry.Via}}
		}
		for hopIndex, hop := range hops {
			device := hop.Dev
			if device == "" {
				device = entry.Dev
			}
			gateway, gatewayErr := routeGateway(hop.Gateway, hop.Via, family)
			weight, weightErr := routeUint32(hop.Weight, 1, false)
			if device == "" || gatewayErr != nil || weightErr != nil || weight < 1 || weight > 256 {
				parseErrors = append(parseErrors, fmt.Sprintf("%s route %d nexthop %d: missing interface or invalid gateway/weight", family, index, hopIndex))
				continue
			}
			result = append(result, interfaceDefaultRoute{device: device, route: DefaultRoute{
				Family: family, Gateway: gateway, Metric: metric, Table: table, Weight: int(weight),
			}})
		}
	}
	return result, parseErrors
}

func routeUint32(raw json.RawMessage, fallback uint32, table bool) (uint32, error) {
	if len(raw) == 0 {
		return fallback, nil
	}
	value := string(raw)
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &value); err != nil {
			return 0, err
		}
		if table {
			switch value {
			case "main":
				return 254, nil
			case "local":
				return 255, nil
			case "default":
				return 253, nil
			case "unspec":
				return 0, nil
			}
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	return uint32(parsed), err
}

func routeGateway(rawGateway json.RawMessage, via json.RawMessage, family string) (string, error) {
	gateway, viaFamily := "", ""
	if len(rawGateway) != 0 {
		if err := json.Unmarshal(rawGateway, &gateway); err != nil || gateway == "" {
			return "", errors.New("invalid gateway address")
		}
	}
	if len(via) != 0 {
		if gateway != "" {
			return "", errors.New("route has both gateway and via")
		}
		var crossFamily struct {
			Family string `json:"family"`
			Host   string `json:"host"`
		}
		if err := json.Unmarshal(via, &crossFamily); err != nil {
			return "", err
		}
		gateway, viaFamily = crossFamily.Host, crossFamily.Family
		if gateway == "" || (viaFamily != "inet" && viaFamily != "inet6") {
			return "", errors.New("invalid cross-family gateway")
		}
	}
	if gateway == "" {
		return "", nil // An explicit device without a gateway is directly connected.
	}
	address, err := netip.ParseAddr(gateway)
	if err != nil || address.Zone() != "" {
		return "", errors.New("invalid gateway address")
	}
	wantIPv4 := family == "ipv4"
	if viaFamily != "" {
		wantIPv4 = viaFamily == "inet"
	}
	if address.Is4() != wantIPv4 {
		return "", errors.New("gateway has incorrect address family")
	}
	return address.String(), nil
}
