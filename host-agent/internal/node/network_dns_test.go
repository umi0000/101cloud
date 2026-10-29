package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseResolvConfSeparatesStubsAndSearch(t *testing.T) {
	system, stub := parseResolvConf([]byte("# systemd-resolved\nnameserver 127.0.0.53\nnameserver 127.0.0.1\nnameserver ::1\nnameserver 192.0.2.53 # upstream\nnameserver 2001:db8::53\nnameserver 192.0.2.53\nsearch old.example\ndomain example.test\nsearch corp.test lab.test corp.test\n"))
	if !stub || !reflect.DeepEqual(system.Servers, []string{"192.0.2.53", "2001:db8::53"}) || !reflect.DeepEqual(system.StubServers, []string{"127.0.0.53", "127.0.0.1", "::1"}) || !reflect.DeepEqual(system.SearchDomains, []string{"corp.test", "lab.test"}) || system.Error != "" {
		t.Fatalf("unexpected resolver configuration: %+v, stub=%v", system, stub)
	}
}

func TestParseResolvConfRejectsMalformedAddresses(t *testing.T) {
	system, stub := parseResolvConf([]byte("nameserver\nnameserver not-an-address\nnameserver 0.0.0.0\nnameserver ff02::1\nnameserver fe80::53\nnameserver 192.0.2.1 extra\nnameserver fe80::53%eth0\n"))
	if stub || !reflect.DeepEqual(system.Servers, []string{"fe80::53%eth0"}) || !strings.Contains(system.Error, "invalid nameserver") || !strings.Contains(system.Error, "malformed") || !strings.Contains(system.Error, "zone") {
		t.Fatalf("bad addresses were not reported or were exposed: %+v", system)
	}
	oversized, _ := parseResolvConf([]byte(strings.Repeat("x", maxDNSFileSize+1)))
	if !strings.Contains(oversized.Error, "64 KiB") || len(oversized.Servers) != 0 {
		t.Fatalf("oversized resolver data accepted: %+v", oversized)
	}
}

func TestDNSFilesOnlyUseResolvedUpstreamForResolvedStub(t *testing.T) {
	for _, tc := range []struct {
		name             string
		config           string
		allowResolved    bool
		wantSource       string
		wantServers      []string
		wantUpstreamRead bool
	}{
		{"resolved", "nameserver 127.0.0.53\nsearch corp.test\n", true, "systemd-resolved-resolv.conf", []string{"192.0.2.53", "2001:db8::53"}, true},
		{"dnsmasq", "nameserver 127.0.0.1\n", true, "resolv.conf", []string{}, false},
		{"loopback-v6", "nameserver ::1\n", true, "resolv.conf", []string{}, false},
		{"ordinary", "nameserver 198.51.100.53\n", true, "resolv.conf", []string{"198.51.100.53"}, false},
		{"non-linux", "nameserver 127.0.0.53\n", false, "resolv.conf", []string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstreamRead := false
			reader := func(_ context.Context, path string) ([]byte, error) {
				switch path {
				case resolvConfPath:
					return []byte(tc.config), nil
				case resolvedResolvConfPath:
					upstreamRead = true
					return []byte("nameserver 192.0.2.53\nnameserver 2001:db8::53\nsearch corp.test\n"), nil
				default:
					t.Fatalf("unexpected source: %s", path)
					return nil, nil
				}
			}
			system := collectDNSFiles(context.Background(), reader, tc.allowResolved)
			if system.Source != tc.wantSource || !reflect.DeepEqual(system.Servers, tc.wantServers) || upstreamRead != tc.wantUpstreamRead {
				t.Fatalf("unexpected fallback: %+v, upstreamRead=%v", system, upstreamRead)
			}
		})
	}
}

func TestDNSMissingUpstreamKeepsStubAndSearch(t *testing.T) {
	reader := func(_ context.Context, path string) ([]byte, error) {
		if path == resolvConfPath {
			return []byte("nameserver 127.0.0.53\nsearch corp.test\n"), nil
		}
		return nil, os.ErrNotExist
	}
	system := collectDNSFiles(context.Background(), reader, true)
	if system.Source != "resolv.conf" || len(system.Servers) != 0 || !reflect.DeepEqual(system.StubServers, []string{"127.0.0.53"}) || !reflect.DeepEqual(system.SearchDomains, []string{"corp.test"}) || !strings.Contains(system.Error, resolvedResolvConfPath) {
		t.Fatalf("missing upstream lost fallback information: %+v", system)
	}
}

func TestDNSUnavailableNeverAssignsSystemServersToLinks(t *testing.T) {
	for _, reason := range []string{"system bus socket missing", "org.freedesktop.DBus.Error.AccessDenied", "org.freedesktop.DBus.Error.ServiceUnknown", "unknown DNS property signature"} {
		t.Run(reason, func(t *testing.T) {
			reader := func(context.Context, string) ([]byte, error) { return []byte("nameserver 192.0.2.53\n"), nil }
			resolved := func(context.Context, []NetworkInterface) (SystemDNS, map[int]InterfaceDNS, error) {
				return SystemDNS{}, nil, errors.New(reason)
			}
			system, links := collectDNSFromSources(context.Background(), []NetworkInterface{{Index: 2}, {Index: 7}}, reader, resolved, "")
			if system.Source != "resolv.conf" || !reflect.DeepEqual(system.Servers, []string{"192.0.2.53"}) || !strings.Contains(system.Error, reason) {
				t.Fatalf("system fallback missing: %+v", system)
			}
			for _, index := range []int{2, 7} {
				if links[index].Source != "unavailable" || len(links[index].Servers) != 0 || !strings.Contains(links[index].Error, reason) {
					t.Fatalf("system DNS falsely attributed to interface %d: %+v", index, links[index])
				}
			}
		})
	}
}

func TestDNSTimeoutRetainsFileFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	reader := func(context.Context, string) ([]byte, error) { return []byte("nameserver 192.0.2.53\n"), nil }
	resolved := func(ctx context.Context, _ []NetworkInterface) (SystemDNS, map[int]InterfaceDNS, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > networkDNSTimeout {
			t.Error("resolved did not receive a bounded context")
		}
		<-ctx.Done()
		return SystemDNS{}, nil, ctx.Err()
	}
	start := time.Now()
	system, links := collectDNSFromSources(ctx, []NetworkInterface{{Index: 2}}, reader, resolved, "")
	if time.Since(start) > time.Second || system.Source != "resolv.conf" || !strings.Contains(system.Error, "deadline exceeded") || links[2].Source != "unavailable" || !reflect.DeepEqual(system.Servers, []string{"192.0.2.53"}) {
		t.Fatalf("deadline/fallback not respected: %+v %+v", system, links)
	}
}

func TestDNSMissingAllSourcesIsExplicit(t *testing.T) {
	reader := func(context.Context, string) ([]byte, error) { return nil, os.ErrNotExist }
	system, links := collectDNSFromSources(context.Background(), []NetworkInterface{{Index: 2}}, reader, nil, "unsupported operating system")
	if system.Source != "unavailable" || system.Servers == nil || system.StubServers == nil || system.SearchDomains == nil || !strings.Contains(system.Error, resolvConfPath) || !strings.Contains(system.Error, "unsupported") || links[2].Source != "unavailable" || links[2].Error == "" {
		t.Fatalf("unknown DNS must be explicit, not fabricated: %+v %+v", system, links)
	}
}

func TestReadDNSFileEnforcesSizeAndContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	data := []byte("nameserver 192.0.2.53\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readDNSFile(context.Background(), path)
	if err != nil || string(got) != string(data) {
		t.Fatalf("regular DNS file: %q %v", got, err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxDNSFileSize+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDNSFile(context.Background(), path); err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("oversized DNS file accepted: %v", err)
	}
	if _, err := readDNSFile(context.Background(), filepath.Dir(path)); err == nil {
		t.Fatal("directory accepted as a DNS file")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readDNSFile(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled file read: %v", err)
	}
}
