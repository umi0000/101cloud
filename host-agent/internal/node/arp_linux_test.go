//go:build linux

package node

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func arpTestPacket(opcode uint16, sender, target [4]byte, mac [6]byte) []byte {
	packet := make([]byte, 42)
	copy(packet[:6], []byte{255, 255, 255, 255, 255, 255})
	copy(packet[6:12], mac[:])
	binary.BigEndian.PutUint16(packet[12:14], 0x0806)
	binary.BigEndian.PutUint16(packet[14:16], 1)
	binary.BigEndian.PutUint16(packet[16:18], 0x0800)
	packet[18], packet[19] = 6, 4
	binary.BigEndian.PutUint16(packet[20:22], opcode)
	copy(packet[22:28], mac[:])
	copy(packet[28:32], sender[:])
	copy(packet[38:42], target[:])
	return packet
}

func TestMakeARPProbeWireFormat(t *testing.T) {
	mac := [6]byte{2, 1, 2, 3, 4, 5}
	target := [4]byte{192, 0, 2, 112}
	packet := makeARPProbe(mac, target)
	if len(packet) != 60 || [6]byte(packet[:6]) != [6]byte{255, 255, 255, 255, 255, 255} || [6]byte(packet[6:12]) != mac {
		t.Fatalf("invalid Ethernet framing: %x", packet)
	}
	if [8]byte(packet[12:20]) != [8]byte{8, 6, 0, 1, 8, 0, 6, 4} || binary.BigEndian.Uint16(packet[20:22]) != 1 {
		t.Fatalf("not an Ethernet/IPv4 ARP request: %x", packet)
	}
	if [6]byte(packet[22:28]) != mac || [4]byte(packet[28:32]) != [4]byte{} || [6]byte(packet[32:38]) != [6]byte{} || [4]byte(packet[38:42]) != target {
		t.Fatalf("ARP probe must use zero sender IPv4/target hardware: %x", packet)
	}
	for _, padding := range packet[42:] {
		if padding != 0 {
			t.Fatal("Ethernet padding must not expose uninitialized data")
		}
	}
}

func TestParseARPConflict(t *testing.T) {
	target := [4]byte{192, 0, 2, 112}
	other := [4]byte{192, 0, 2, 1}
	remote := [6]byte{2, 0, 0, 0, 0, 2}
	local := [6]byte{2, 0, 0, 0, 0, 1}
	localMACs := map[[6]byte]struct{}{local: {}}
	tests := []struct {
		name        string
		opcode      uint16
		sender      [4]byte
		destination [4]byte
		mac         [6]byte
		packetType  int
		mutate      func([]byte)
		want        string
	}{
		{name: "reply claiming target", opcode: 2, sender: target, destination: other, mac: remote, want: "arp_reply"},
		{name: "request claiming target", opcode: 1, sender: target, destination: other, mac: remote, want: "arp_request"},
		{name: "gratuitous request", opcode: 1, sender: target, destination: target, mac: remote, want: "arp_request"},
		{name: "simultaneous zero sender probe", opcode: 1, destination: target, mac: remote, want: "arp_probe"},
		{name: "outgoing frame", opcode: 2, sender: target, mac: remote, packetType: unix.PACKET_OUTGOING},
		{name: "own hardware reflected", opcode: 2, sender: target, mac: local},
		{name: "own concurrent probe", opcode: 1, destination: target, mac: local},
		{name: "unrelated sender querying target", opcode: 1, sender: other, destination: target, mac: remote},
		{name: "unrelated reply", opcode: 2, sender: other, destination: target, mac: remote},
		{name: "unrelated concurrent probe", opcode: 1, destination: other, mac: remote},
		{name: "zero sender reply not a probe", opcode: 2, destination: target, mac: remote},
		{name: "unsupported opcode", opcode: 3, sender: target, mac: remote},
		{name: "zero hardware", opcode: 2, sender: target},
		{name: "multicast hardware", opcode: 2, sender: target, mac: [6]byte{1, 0, 0, 0, 0, 1}},
		{name: "Ethernet source differs", opcode: 2, sender: target, mac: remote, mutate: func(packet []byte) { packet[6] ^= 4 }},
		{name: "not Ethernet ARP", opcode: 2, sender: target, mac: remote, mutate: func(packet []byte) { packet[13] = 0 }},
		{name: "wrong hardware type", opcode: 2, sender: target, mac: remote, mutate: func(packet []byte) { packet[15] = 2 }},
		{name: "wrong protocol type", opcode: 2, sender: target, mac: remote, mutate: func(packet []byte) { packet[16] = 0x86; packet[17] = 0xdd }},
		{name: "wrong hardware length", opcode: 2, sender: target, mac: remote, mutate: func(packet []byte) { packet[18] = 8 }},
		{name: "wrong protocol length", opcode: 2, sender: target, mac: remote, mutate: func(packet []byte) { packet[19] = 16 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet := arpTestPacket(test.opcode, test.sender, test.destination, test.mac)
			if test.mutate != nil {
				test.mutate(packet)
			}
			mac, source, conflict := parseARPConflict(packet, test.packetType, target, localMACs)
			if conflict != (test.want != "") || source != test.want || (conflict && mac != test.mac) {
				t.Fatalf("conflict=%v mac=%x source=%q; want %q", conflict, mac, source, test.want)
			}
		})
	}
	packet := arpTestPacket(2, target, other, remote)
	for length := range 42 {
		if _, _, conflict := parseARPConflict(packet[:length], 0, target, nil); conflict {
			t.Fatalf("truncated packet length %d accepted", length)
		}
	}
}

func TestARPResponderDeduplicationAndBound(t *testing.T) {
	var seen arpResponders
	result := newIPProbeResult(IPProbeRequest{})
	for index := range 1000 {
		mac := [6]byte{2, 0, 0, 0, byte(index >> 8), byte(index)}
		seen.add(&result, mac, "arp_probe")
		seen.add(&result, mac, "arp_reply")
	}
	if len(result.Responders) != maxProbeResponders || seen.count != maxProbeResponders {
		t.Fatalf("unbounded response collection: %d entries", len(result.Responders))
	}
	unique := make(map[string]bool)
	for _, responder := range result.Responders {
		if unique[responder.MAC] || responder.Source != "arp_probe" {
			t.Fatalf("duplicate MAC or overwritten initial evidence: %+v", responder)
		}
		unique[responder.MAC] = true
	}
	result = finishIPProbe(result)
	if result.Status != IPProbeStatusOccupied || result.Available == nil || *result.Available {
		t.Fatalf("bounded evidence must still prove occupancy: %+v", result)
	}
}

func TestProbeLinkUsable(t *testing.T) {
	original := probeTestInterface("eth0", "192.0.2.10/24").iface
	tests := []struct {
		name   string
		modify func(*net.Interface)
		want   bool
	}{
		{name: "unchanged", want: true},
		{name: "carrier lost", modify: func(iface *net.Interface) { iface.Flags &^= net.FlagRunning }},
		{name: "down", modify: func(iface *net.Interface) { iface.Flags &^= net.FlagUp }},
		{name: "renamed", modify: func(iface *net.Interface) { iface.Name = "eth1" }},
		{name: "recreated", modify: func(iface *net.Interface) { iface.Index++ }},
		{name: "hardware changed", modify: func(iface *net.Interface) { iface.HardwareAddr = net.HardwareAddr{2, 0, 0, 0, 0, 3} }},
		{name: "hardware absent", modify: func(iface *net.Interface) { iface.HardwareAddr = nil }},
		{name: "loopback", modify: func(iface *net.Interface) { iface.Flags |= net.FlagLoopback }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := original
			if test.modify != nil {
				test.modify(&current)
			}
			if got := probeLinkUsable(original, current); got != test.want {
				t.Fatalf("link usability=%v, want %v", got, test.want)
			}
		})
	}
}

func probeTestLinkAttribute(kind uint16, value []byte) []byte {
	length := unix.SizeofRtAttr + len(value)
	attribute := make([]byte, (length+3)&^3)
	binary.NativeEndian.PutUint16(attribute[:2], uint16(length))
	binary.NativeEndian.PutUint16(attribute[2:4], kind)
	copy(attribute[unix.SizeofRtAttr:], value)
	return attribute
}

func probeTestLinkMessage(index int, kind uint16, flags uint32, attributes []byte) []byte {
	message := make([]byte, unix.NLMSG_HDRLEN+unix.SizeofIfInfomsg+len(attributes))
	binary.NativeEndian.PutUint32(message[:4], uint32(len(message)))
	binary.NativeEndian.PutUint16(message[4:6], kind)
	binary.NativeEndian.PutUint32(message[unix.NLMSG_HDRLEN+4:], uint32(index))
	binary.NativeEndian.PutUint32(message[unix.NLMSG_HDRLEN+8:], flags)
	copy(message[unix.NLMSG_HDRLEN+unix.SizeofIfInfomsg:], attributes)
	return message
}

func TestProbeLinkNotifications(t *testing.T) {
	iface := probeTestInterface("eth0", "192.0.2.10/24").iface
	up := uint32(unix.IFF_UP | unix.IFF_RUNNING)
	tests := []struct {
		name    string
		message []byte
		want    bool
	}{
		{name: "up carrier", message: probeTestLinkMessage(iface.Index, unix.RTM_NEWLINK, up, probeTestLinkAttribute(unix.IFLA_CARRIER, []byte{1})), want: true},
		{name: "same hardware", message: probeTestLinkMessage(iface.Index, unix.RTM_NEWLINK, up, probeTestLinkAttribute(unix.IFLA_ADDRESS, iface.HardwareAddr)), want: true},
		{name: "down unrelated interface", message: probeTestLinkMessage(99, unix.RTM_NEWLINK, 0, nil), want: true},
		{name: "down", message: probeTestLinkMessage(iface.Index, unix.RTM_NEWLINK, 0, nil)},
		{name: "carrier zero", message: probeTestLinkMessage(iface.Index, unix.RTM_NEWLINK, up, probeTestLinkAttribute(unix.IFLA_CARRIER, []byte{0}))},
		{name: "deleted", message: probeTestLinkMessage(iface.Index, unix.RTM_DELLINK, up, nil)},
		{name: "hardware changed", message: probeTestLinkMessage(iface.Index, unix.RTM_NEWLINK, up, probeTestLinkAttribute(unix.IFLA_ADDRESS, []byte{2, 0, 0, 0, 0, 9}))},
		{name: "malformed carrier", message: probeTestLinkMessage(iface.Index, unix.RTM_NEWLINK, up, probeTestLinkAttribute(unix.IFLA_CARRIER, nil))},
		{name: "malformed attribute", message: probeTestLinkMessage(iface.Index, unix.RTM_NEWLINK, up, []byte{1, 0, 0, 0})},
		{name: "short header", message: []byte{1, 2, 3}},
		{name: "monitor error", message: probeTestLinkMessage(0, unix.NLMSG_ERROR, 0, nil)},
		{name: "overrun", message: probeTestLinkMessage(0, unix.NLMSG_OVERRUN, 0, nil)},
		{name: "down then up remains unsafe", message: append(probeTestLinkMessage(iface.Index, unix.RTM_NEWLINK, 0, nil), probeTestLinkMessage(iface.Index, unix.RTM_NEWLINK, up, nil)...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := probeLinkMessagesUsable(test.message, iface); got != test.want {
				t.Fatalf("link notification usable=%v; want %v", got, test.want)
			}
		})
	}
}

func TestProbePollCancellation(t *testing.T) {
	// Exercise the actual nonblocking socket/poll boundary without CAP_NET_RAW.
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(sockets[0])
	defer unix.Close(sockets[1])
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	descriptors := []unix.PollFd{{Fd: int32(sockets[0]), Events: unix.POLLIN}}
	for {
		err = waitProbeEvents(ctx, descriptors, start.Add(time.Second))
		if err != nil {
			break
		}
		if time.Since(start) > time.Second {
			t.Fatal("poll did not observe cancellation")
		}
	}
	if !errors.Is(err, context.Canceled) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("cancellation not promptly observed: elapsed=%v err=%v", time.Since(start), err)
	}
}

func TestProbeSocketFailuresRemainUnknown(t *testing.T) {
	for _, socketErr := range []error{unix.EPERM, unix.EACCES, unix.EIO, unix.ENOBUFS} {
		result, err := probeSocketFailure(finishIPProbe(newIPProbeResult(IPProbeRequest{})), socketErr)
		var probeErr *ProbeError
		if !errors.As(err, &probeErr) || probeErr.Status != 503 || result.Status != IPProbeStatusUnknown || result.Available != nil {
			t.Fatalf("socket failure must not infer availability: %+v %v", result, err)
		}
		if (errors.Is(socketErr, unix.EPERM) || errors.Is(socketErr, unix.EACCES)) && probeErr.Code != "socket_permission_denied" {
			t.Fatalf("permission failure code: %s", probeErr.Code)
		}
	}
}
