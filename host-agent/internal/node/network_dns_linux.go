//go:build linux

package node

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const dnsNonblockFlag = unix.O_NONBLOCK

// The bus address is deliberately fixed. In particular, neither
// DBUS_SYSTEM_BUS_ADDRESS nor a caller-provided address is trusted.
const resolvedSystemBusSocket = "/run/dbus/system_bus_socket"

// These field orders match resolve1.Manager's a(iiay) and a(isb) properties.
// Manager.DNS is aggregated across links; Index is essential scope information.
type resolvedDNSServer struct {
	Index   int32
	Family  int32
	Address []byte
}

type resolvedDNSDomain struct {
	Index     int32
	Domain    string
	RouteOnly bool
}

func collectNetworkDNS(ctx context.Context, interfaces []NetworkInterface) (SystemDNS, map[int]InterfaceDNS) {
	return collectDNSFromSources(ctx, interfaces, readDNSFile, readResolvedDNS, "")
}

func readResolvedDNS(ctx context.Context, interfaces []NetworkInterface) (SystemDNS, map[int]InterfaceDNS, error) {
	// This deadline covers dial, authentication, Hello and both property reads.
	// NewConn's context closes the underlying socket on cancellation, while
	// SetDeadline also bounds synchronous authentication I/O.
	socket, err := (&net.Dialer{}).DialContext(ctx, "unix", resolvedSystemBusSocket)
	if err != nil {
		return SystemDNS{}, nil, fmt.Errorf("systemd-resolved system bus connection: %w", err)
	}
	defer socket.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := socket.SetDeadline(deadline); err != nil {
			return SystemDNS{}, nil, fmt.Errorf("system bus deadline: %w", err)
		}
	}
	conn, err := dbus.NewConn(socket, dbus.WithContext(ctx))
	if err != nil {
		return SystemDNS{}, nil, fmt.Errorf("system bus connection: %w", err)
	}
	defer conn.Close()
	// Do not fall back to cookie authentication, which reads a user's home.
	if err := conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Geteuid()))}); err != nil {
		return SystemDNS{}, nil, fmt.Errorf("system bus authentication: %w", err)
	}
	if err := conn.Hello(); err != nil {
		return SystemDNS{}, nil, fmt.Errorf("system bus registration: %w", err)
	}

	object := conn.Object("org.freedesktop.resolve1", "/org/freedesktop/resolve1")
	var dnsVariant dbus.Variant
	if err := object.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", dbus.FlagNoAutoStart, "org.freedesktop.resolve1.Manager", "DNS").Store(&dnsVariant); err != nil {
		return SystemDNS{}, nil, fmt.Errorf("systemd-resolved DNS unavailable: %w", err)
	}
	var servers []resolvedDNSServer
	if dnsVariant.Signature().String() != "a(iiay)" {
		return SystemDNS{}, nil, fmt.Errorf("systemd-resolved DNS has unexpected signature %s", dnsVariant.Signature())
	}
	if err := dbus.Store([]interface{}{dnsVariant.Value()}, &servers); err != nil {
		return SystemDNS{}, nil, fmt.Errorf("systemd-resolved DNS decoding: %w", err)
	}

	var domainsVariant dbus.Variant
	var domains []resolvedDNSDomain
	var domainsErr error
	if err := object.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", dbus.FlagNoAutoStart, "org.freedesktop.resolve1.Manager", "Domains").Store(&domainsVariant); err != nil {
		domainsErr = fmt.Errorf("systemd-resolved search domains unavailable: %w", err)
	} else if domainsVariant.Signature().String() != "a(isb)" {
		domainsErr = fmt.Errorf("systemd-resolved Domains has unexpected signature %s", domainsVariant.Signature())
	} else if err := dbus.Store([]interface{}{domainsVariant.Value()}, &domains); err != nil {
		domainsErr = fmt.Errorf("systemd-resolved Domains decoding: %w", err)
	}
	system, links := decodeResolvedDNS(interfaces, servers, domains)
	if domainsErr != nil {
		system.Error = joinDNSError(system.Error, domainsErr.Error())
	}
	return system, links, nil
}

func decodeResolvedDNS(interfaces []NetworkInterface, servers []resolvedDNSServer, domains []resolvedDNSDomain) (SystemDNS, map[int]InterfaceDNS) {
	system := emptySystemDNS()
	system.Source = "systemd-resolved"
	links := make(map[int]InterfaceDNS, len(interfaces))
	names := make(map[int]string, len(interfaces))
	for _, iface := range interfaces {
		names[iface.Index] = iface.Name
		links[iface.Index] = InterfaceDNS{Servers: []string{}, Source: "unavailable"}
	}
	for _, server := range servers {
		index := int(server.Index)
		address, err := decodeResolvedAddress(server, names[index])
		if err != nil {
			if index == 0 {
				system.Error = joinDNSError(system.Error, err.Error())
			} else if link, ok := links[index]; ok {
				link.Error = joinDNSError(link.Error, err.Error())
				links[index] = link
			} else {
				system.Error = joinDNSError(system.Error, err.Error())
			}
			continue
		}
		if index == 0 {
			if address.IsLoopback() {
				system.StubServers = appendUniqueDNS(system.StubServers, address.String())
			} else {
				system.Servers = appendUniqueDNS(system.Servers, address.String())
			}
			continue
		}
		link, ok := links[index]
		if !ok {
			// An interface may disappear between netlink and DBus snapshots.
			// Never reinterpret its DNS as global or another link's DNS.
			continue
		}
		if address.IsLoopback() {
			system.StubServers = appendUniqueDNS(system.StubServers, address.String())
			link.Error = joinDNSError(link.Error, "per-link DNS is a host-local loopback resolver, not a container upstream")
		} else {
			link.Source = "systemd-resolved"
			link.Servers = appendUniqueDNS(link.Servers, address.String())
		}
		links[index] = link
	}
	for index, link := range links {
		if link.Source == "unavailable" && link.Error == "" {
			link.Error = "systemd-resolved reported no per-link DNS for this interface"
			links[index] = link
		}
	}
	for _, domain := range domains {
		if domain.Index != 0 || domain.RouteOnly || domain.Domain == "" || domain.Domain == "." {
			continue
		}
		// Do not attribute another interface's search domains to global config.
		// Per-link domains are not part of this system-level field.
		system.SearchDomains = appendUniqueDNS(system.SearchDomains, domain.Domain)
	}
	return system, links
}

func decodeResolvedAddress(server resolvedDNSServer, name string) (netip.Addr, error) {
	var address netip.Addr
	switch server.Family {
	case unix.AF_INET:
		if len(server.Address) != 4 {
			return address, fmt.Errorf("systemd-resolved invalid IPv4 DNS length on interface %d", server.Index)
		}
		address = netip.AddrFrom4([4]byte(server.Address))
	case unix.AF_INET6:
		if len(server.Address) != 16 {
			return address, fmt.Errorf("systemd-resolved invalid IPv6 DNS length on interface %d", server.Index)
		}
		address = netip.AddrFrom16([16]byte(server.Address)).Unmap()
	default:
		return address, fmt.Errorf("systemd-resolved unsupported DNS address family %d", server.Family)
	}
	if server.Index < 0 || address.IsUnspecified() || address.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("systemd-resolved invalid DNS address on interface %d", server.Index)
	}
	if address.Is6() && address.IsLinkLocalUnicast() {
		if server.Index == 0 || name == "" {
			return netip.Addr{}, fmt.Errorf("systemd-resolved link-local IPv6 DNS has no known interface zone (index %d)", server.Index)
		}
		address = address.WithZone(name)
	}
	return address, nil
}
