package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"
)

const (
	networkDNSTimeout      = 2 * time.Second
	maxDNSFileSize         = 64 * 1024
	resolvConfPath         = "/etc/resolv.conf"
	resolvedResolvConfPath = "/run/systemd/resolve/resolv.conf"
)

type dnsFileReader func(context.Context, string) ([]byte, error)
type resolvedDNSReader func(context.Context, []NetworkInterface) (SystemDNS, map[int]InterfaceDNS, error)

func emptySystemDNS() SystemDNS {
	return SystemDNS{Servers: []string{}, StubServers: []string{}, SearchDomains: []string{}, Source: "unavailable"}
}

// collectDNSFromSources reads file fallback before contacting resolved so a slow
// or absent bus cannot prevent returning already available system configuration.
func collectDNSFromSources(ctx context.Context, interfaces []NetworkInterface, readFile dnsFileReader, readResolved resolvedDNSReader, unsupportedReason string) (SystemDNS, map[int]InterfaceDNS) {
	ctx, cancel := context.WithTimeout(ctx, networkDNSTimeout)
	defer cancel()
	system := collectDNSFiles(ctx, readFile, readResolved != nil)
	links := make(map[int]InterfaceDNS, len(interfaces))
	var resolvedErr error
	if readResolved != nil {
		resolved, perLink, err := readResolved(ctx, interfaces)
		if err == nil {
			// File-level stubs describe the local resolver entry point, not
			// resolved's upstream DNS, and remain useful alongside DBus data.
			resolved.StubServers = appendUniqueDNS(resolved.StubServers, system.StubServers...)
			return resolved, perLink
		}
		resolvedErr = err
	} else {
		resolvedErr = errors.New(unsupportedReason)
	}
	reason := "per-interface DNS unavailable: " + resolvedErr.Error()
	for _, iface := range interfaces {
		links[iface.Index] = InterfaceDNS{Servers: []string{}, Source: "unavailable", Error: reason}
	}
	system.Error = joinDNSError(system.Error, resolvedErr.Error())
	return system, links
}

func collectDNSFiles(ctx context.Context, readFile dnsFileReader, allowResolvedFallback bool) SystemDNS {
	system := emptySystemDNS()
	data, err := readFile(ctx, resolvConfPath)
	if err != nil {
		system.Error = fmt.Sprintf("read %s: %v", resolvConfPath, err)
		return system
	}
	system, resolvedStub := parseResolvConf(data)
	system.Source = "resolv.conf"
	if !resolvedStub || !allowResolvedFallback {
		return system
	}
	upstream, err := readFile(ctx, resolvedResolvConfPath)
	if err != nil {
		system.Error = joinDNSError(system.Error, fmt.Sprintf("read %s: %v", resolvedResolvConfPath, err))
		return system
	}
	parsed, _ := parseResolvConf(upstream)
	system.Servers = appendUniqueDNS(system.Servers, parsed.Servers...)
	system.StubServers = appendUniqueDNS(system.StubServers, parsed.StubServers...)
	system.SearchDomains = appendUniqueDNS(system.SearchDomains, parsed.SearchDomains...)
	system.Source = "systemd-resolved-resolv.conf"
	system.Error = joinDNSError(system.Error, parsed.Error)
	return system
}

// readDNSFile accepts only bounded regular files. The caller's deadline also
// bounds opening/statting a file, including a broken or unusual mount.
func readDNSFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		file, err := os.OpenFile(path, os.O_RDONLY|dnsNonblockFlag, 0)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			done <- result{err: err}
			return
		}
		if !info.Mode().IsRegular() {
			done <- result{err: errors.New("DNS source is not a regular file")}
			return
		}
		if info.Size() > maxDNSFileSize {
			done <- result{err: errors.New("DNS source exceeds 64 KiB")}
			return
		}
		data, err := io.ReadAll(io.LimitReader(file, maxDNSFileSize+1))
		if len(data) > maxDNSFileSize {
			data, err = nil, errors.New("DNS source exceeds 64 KiB")
		}
		done <- result{data: data, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-done:
		return result.data, result.err
	}
}

func parseResolvConf(data []byte) (SystemDNS, bool) {
	system := emptySystemDNS()
	if len(data) > maxDNSFileSize {
		system.Error = "DNS source exceeds 64 KiB"
		return system, false
	}
	resolvedAddress := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if comment := strings.IndexAny(line, "#;"); comment >= 0 {
			line = line[:comment]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			if len(fields) != 2 {
				system.Error = joinDNSError(system.Error, "malformed nameserver directive")
				continue
			}
			address, err := netip.ParseAddr(fields[1])
			address = address.Unmap()
			if err != nil || address.IsUnspecified() || address.IsMulticast() {
				system.Error = joinDNSError(system.Error, "invalid nameserver address")
				continue
			}
			if address.IsLoopback() {
				system.StubServers = appendUniqueDNS(system.StubServers, address.String())
				resolvedAddress = resolvedAddress || address.String() == "127.0.0.53"
			} else if address.Is6() && address.IsLinkLocalUnicast() && address.Zone() == "" {
				system.Error = joinDNSError(system.Error, "link-local IPv6 nameserver has no interface zone")
			} else {
				system.Servers = appendUniqueDNS(system.Servers, address.String())
			}
		case "search", "domain":
			// resolv.conf uses the last search/domain directive, not a union.
			system.SearchDomains = []string{}
			if fields[0] == "domain" && len(fields) > 2 {
				fields = fields[:2]
			}
			system.SearchDomains = appendUniqueDNS(system.SearchDomains, fields[1:]...)
		}
	}
	return system, resolvedAddress
}

func appendUniqueDNS(values []string, additions ...string) []string {
	for _, addition := range additions {
		found := false
		for _, value := range values {
			if value == addition {
				found = true
				break
			}
		}
		if !found {
			values = append(values, addition)
		}
	}
	return values
}

func joinDNSError(existing, addition string) string {
	if addition == "" || existing == addition || strings.Contains(existing, addition) {
		return existing
	}
	if existing == "" {
		return addition
	}
	return existing + "; " + addition
}
