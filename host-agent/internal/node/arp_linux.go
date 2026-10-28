//go:build linux

package node

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

const (
	arpProbeCount        = 3
	arpProbeInterval     = time.Second
	arpPollInterval      = 50 * time.Millisecond
	maxProbeResponders   = 32
	maxProbePackets      = 8192
	maxProbeLinkMessages = 1024
)

func probeIPv4(ctx context.Context, req IPProbeRequest) (IPProbeResult, error) {
	result := newIPProbeResult(req)
	if err := ctx.Err(); err != nil {
		return probeContextFailure(result, err)
	}
	network, result, err := prepareIPProbe(req)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return probeContextFailure(result, err)
	}
	if result.Status == IPProbeStatusOccupied {
		return result, nil
	}
	if !probeLinkUsable(network.iface, network.iface) {
		return probeFailure(result, 503, "link_unavailable", "Interface has no usable Ethernet carrier")
	}

	// Subscribe before taking a second snapshot so a down/up transition during
	// the observation cannot be mistaken for uninterrupted carrier.
	linkFD, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return probeSocketFailure(result, err)
	}
	defer unix.Close(linkFD)
	if err := unix.Bind(linkFD, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: unix.RTMGRP_LINK}); err != nil {
		return probeSocketFailure(result, err)
	}
	if err := checkProbeLink(network.iface); err != nil {
		return probeFailure(result, 503, "link_unavailable", "Interface has no usable Ethernet carrier")
	}

	var protocolBytes [2]byte
	binary.BigEndian.PutUint16(protocolBytes[:], unix.ETH_P_ARP)
	protocol := int(binary.NativeEndian.Uint16(protocolBytes[:]))
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, protocol)
	if err != nil {
		return probeSocketFailure(result, err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: uint16(protocol), Ifindex: network.iface.Index}); err != nil {
		return probeSocketFailure(result, err)
	}
	frame := makeARPProbe([6]byte(network.iface.HardwareAddr), network.target.As4())
	destination := &unix.SockaddrLinklayer{
		Protocol: uint16(protocol),
		Ifindex:  network.iface.Index,
		Halen:    6,
		Addr:     [8]byte{255, 255, 255, 255, 255, 255},
	}
	pollFDs := []unix.PollFd{
		{Fd: int32(fd), Events: unix.POLLIN},
		{Fd: int32(linkFD), Events: unix.POLLIN},
	}
	start := time.Now()
	end := start.Add(arpProbeCount * arpProbeInterval)
	nextSend := start
	sent := 0
	packets := 0
	linkMessages := 0
	var packet [2048]byte
	var linkBuffer [16384]byte
	var responders arpResponders
	for {
		if err := ctx.Err(); err != nil {
			return probeContextFailure(result, err)
		}
		now := time.Now()
		finished := !now.Before(end)
		if finished {
			if sent != arpProbeCount {
				return probeFailure(result, 503, "probe_incomplete", "The fixed ARP observation window elapsed before all probes could be sent")
			}
			if err := checkProbeLink(network.iface); err != nil {
				return probeFailure(result, 503, "link_changed", "Interface link state changed during the probe")
			}
		}
		if sent < arpProbeCount && !now.Before(nextSend) {
			if err := unix.Sendto(fd, frame[:], 0, destination); err != nil {
				return probeSocketFailure(result, err)
			}
			sent++
			nextSend = start.Add(time.Duration(sent) * arpProbeInterval)
		}
		wake := end
		if sent < arpProbeCount && nextSend.Before(wake) {
			wake = nextSend
		}
		if err := waitProbeEvents(ctx, pollFDs, wake); err != nil {
			if ctx.Err() != nil {
				return probeContextFailure(result, ctx.Err())
			}
			return probeSocketFailure(result, err)
		}
		for _, polled := range pollFDs {
			if polled.Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				return probeFailure(result, 503, "socket_error", "Probe socket became unavailable")
			}
		}
		// Always process link notifications before considering successful silence.
		// Fixed batches and total limits bound CPU use even under an ARP flood.
		linkDrained := false
		for range 32 {
			n, _, flags, _, err := unix.Recvmsg(linkFD, linkBuffer[:], nil, unix.MSG_DONTWAIT)
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				linkDrained = true
				break
			}
			if err != nil {
				if errors.Is(err, unix.EINTR) {
					continue
				}
				return probeSocketFailure(result, err)
			}
			linkMessages++
			if flags&unix.MSG_TRUNC != 0 || linkMessages > maxProbeLinkMessages {
				return probeFailure(result, 503, "link_monitor_overflow", "Cannot reliably observe interface link changes")
			}
			if !probeLinkMessagesUsable(linkBuffer[:n], network.iface) {
				return probeFailure(result, 503, "link_changed", "Interface link state changed during the probe")
			}
		}
		if !linkDrained {
			return probeFailure(result, 503, "link_monitor_overflow", "Cannot reliably observe interface link changes")
		}
		packetsDrained := false
		for range 32 {
			n, from, err := unix.Recvfrom(fd, packet[:], unix.MSG_DONTWAIT)
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				packetsDrained = true
				break
			}
			if err != nil {
				if errors.Is(err, unix.EINTR) {
					continue
				}
				return probeSocketFailure(result, err)
			}
			packets++
			if packets > maxProbePackets {
				return probeFailure(result, 503, "traffic_limit", "ARP traffic exceeded the bounded observation capacity")
			}
			link, ok := from.(*unix.SockaddrLinklayer)
			if !ok || link.Ifindex != network.iface.Index {
				continue
			}
			mac, source, conflict := parseARPConflict(packet[:n], int(link.Pkttype), network.target.As4(), network.localMACs)
			if !conflict {
				continue
			}
			responders.add(&result, mac, source)
		}
		if !packetsDrained && !time.Now().Before(end) {
			return probeFailure(result, 503, "traffic_limit", "ARP traffic exceeded the bounded observation capacity")
		}
		if finished {
			statistics, err := unix.GetsockoptTpacketStats(fd, unix.SOL_PACKET, unix.PACKET_STATISTICS)
			if err != nil {
				return probeSocketFailure(result, err)
			}
			if statistics.Drops != 0 {
				return probeFailure(result, 503, "receive_overflow", "ARP packets were dropped during the observation")
			}
			if err := ctx.Err(); err != nil {
				return probeContextFailure(result, err)
			}
			return finishIPProbe(result), nil
		}
	}
}

type arpResponders struct {
	macs  [maxProbeResponders][6]byte
	count int
}

func (seen *arpResponders) add(result *IPProbeResult, mac [6]byte, source string) {
	for _, known := range seen.macs[:seen.count] {
		if known == mac {
			return
		}
	}
	if seen.count == maxProbeResponders {
		return
	}
	seen.macs[seen.count] = mac
	seen.count++
	result.Responders = append(result.Responders, IPResponder{MAC: net.HardwareAddr(mac[:]).String(), Source: source})
}

func probeSocketFailure(result IPProbeResult, err error) (IPProbeResult, error) {
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		return probeFailure(result, 503, "socket_permission_denied", "ARP probing requires packet socket permission (CAP_NET_RAW)")
	}
	return probeFailure(result, 503, "socket_error", "ARP probe socket operation failed")
}

func waitProbeEvents(ctx context.Context, descriptors []unix.PollFd, until time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	remaining := time.Until(until)
	if remaining > arpPollInterval {
		remaining = arpPollInterval
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < remaining {
		remaining = time.Until(deadline)
	}
	milliseconds := 0
	if remaining > 0 {
		milliseconds = int((remaining + time.Millisecond - 1) / time.Millisecond)
	}
	_, err := unix.Poll(descriptors, milliseconds)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, unix.EINTR) {
		return nil
	}
	return err
}

func probeLinkUsable(original, current net.Interface) bool {
	return current.Index == original.Index && current.Name == original.Name &&
		current.Flags&(net.FlagUp|net.FlagRunning) == net.FlagUp|net.FlagRunning &&
		current.Flags&net.FlagLoopback == 0 && len(current.HardwareAddr) == 6 &&
		len(original.HardwareAddr) == 6 && [6]byte(current.HardwareAddr) == [6]byte(original.HardwareAddr)
}

func checkProbeLink(original net.Interface) error {
	current, err := net.InterfaceByIndex(original.Index)
	if err != nil {
		return err
	}
	if !probeLinkUsable(original, *current) {
		return errors.New("interface carrier or identity changed")
	}
	return nil
}

// probeLinkMessagesUsable reads native-endian rtnetlink link notifications.
// Lost or malformed monitor data is uncertainty, never evidence of availability.
func probeLinkMessagesUsable(messages []byte, original net.Interface) bool {
	for len(messages) != 0 {
		if len(messages) < unix.NLMSG_HDRLEN {
			return false
		}
		length := int(binary.NativeEndian.Uint32(messages[:4]))
		kind := binary.NativeEndian.Uint16(messages[4:6])
		if length < unix.NLMSG_HDRLEN || length > len(messages) {
			return false
		}
		payload := messages[unix.NLMSG_HDRLEN:length]
		switch kind {
		case unix.NLMSG_ERROR, unix.NLMSG_OVERRUN:
			return false
		case unix.RTM_NEWLINK, unix.RTM_DELLINK:
			if len(payload) < unix.SizeofIfInfomsg {
				return false
			}
			index := int(int32(binary.NativeEndian.Uint32(payload[4:8])))
			if index == original.Index {
				flags := binary.NativeEndian.Uint32(payload[8:12])
				if kind == unix.RTM_DELLINK || flags&(unix.IFF_UP|unix.IFF_RUNNING) != unix.IFF_UP|unix.IFF_RUNNING || flags&unix.IFF_LOOPBACK != 0 {
					return false
				}
				attributes := payload[unix.SizeofIfInfomsg:]
				for len(attributes) != 0 {
					if len(attributes) < unix.SizeofRtAttr {
						return false
					}
					attributeLength := int(binary.NativeEndian.Uint16(attributes[:2]))
					attributeType := binary.NativeEndian.Uint16(attributes[2:4]) & 0x3fff
					if attributeLength < unix.SizeofRtAttr || attributeLength > len(attributes) {
						return false
					}
					value := attributes[unix.SizeofRtAttr:attributeLength]
					if attributeType == unix.IFLA_CARRIER && (len(value) != 1 || value[0] != 1) {
						return false
					}
					if attributeType == unix.IFLA_ADDRESS && (len(value) != 6 || [6]byte(value) != [6]byte(original.HardwareAddr)) {
						return false
					}
					aligned := (attributeLength + 3) &^ 3
					if aligned > len(attributes) {
						return false
					}
					attributes = attributes[aligned:]
				}
			}
		}
		aligned := (length + 3) &^ 3
		if aligned > len(messages) {
			return false
		}
		messages = messages[aligned:]
	}
	return true
}

func makeARPProbe(mac [6]byte, target [4]byte) [60]byte {
	var packet [60]byte
	for index := range 6 {
		packet[index] = 255
	}
	copy(packet[6:12], mac[:])
	binary.BigEndian.PutUint16(packet[12:14], unix.ETH_P_ARP)
	binary.BigEndian.PutUint16(packet[14:16], 1)
	binary.BigEndian.PutUint16(packet[16:18], unix.ETH_P_IP)
	packet[18], packet[19] = 6, 4
	binary.BigEndian.PutUint16(packet[20:22], 1)
	copy(packet[22:28], mac[:])
	// Sender IPv4 and target hardware addresses deliberately remain zero.
	copy(packet[38:42], target[:])
	return packet
}

func parseARPConflict(packet []byte, packetType int, target [4]byte, localMACs map[[6]byte]struct{}) ([6]byte, string, bool) {
	var empty [6]byte
	if packetType == unix.PACKET_OUTGOING || len(packet) < 42 ||
		binary.BigEndian.Uint16(packet[12:14]) != unix.ETH_P_ARP ||
		binary.BigEndian.Uint16(packet[14:16]) != 1 ||
		binary.BigEndian.Uint16(packet[16:18]) != unix.ETH_P_IP || packet[18] != 6 || packet[19] != 4 {
		return empty, "", false
	}
	opcode := binary.BigEndian.Uint16(packet[20:22])
	if opcode != 1 && opcode != 2 {
		return empty, "", false
	}
	mac := [6]byte(packet[22:28])
	if mac == empty || mac[0]&1 != 0 || [6]byte(packet[6:12]) != mac {
		return empty, "", false
	}
	if _, local := localMACs[mac]; local {
		return empty, "", false
	}
	sender := [4]byte(packet[28:32])
	if sender == target {
		if opcode == 2 {
			return mac, "arp_reply", true
		}
		return mac, "arp_request", true
	}
	if opcode == 1 && sender == [4]byte{} && [4]byte(packet[38:42]) == target {
		return mac, "arp_probe", true
	}
	return empty, "", false
}
