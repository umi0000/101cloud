package node

import (
	"context"
	"errors"
	"net"
	"net/netip"
)

const (
	IPProbeStatusOccupied           = "occupied"
	IPProbeStatusNoConflictDetected = "no_conflict_detected"
	IPProbeStatusUnknown            = "unknown"
	IPProbeWarning                  = "ARP probing is a point-in-time observation; it does not reserve the address or guarantee DHCP safety."
)

type IPProbeRequest struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
}

type IPResponder struct {
	MAC    string `json:"mac"`
	Source string `json:"source"`
}

type IPProbeResult struct {
	Interface  string        `json:"interface"`
	Address    string        `json:"address"`
	Status     string        `json:"status"`
	Available  *bool         `json:"available"`
	Responders []IPResponder `json:"responders"`
	Reason     string        `json:"reason,omitempty"`
	Warning    string        `json:"warning"`
}

// ProbeError describes why an observation could not establish a result.
// Its status is for the HTTP boundary; callers must preserve unknown availability.
type ProbeError struct {
	Status  int
	Code    string
	Message string
}

func (err *ProbeError) Error() string { return err.Message }

func newIPProbeResult(req IPProbeRequest) IPProbeResult {
	return IPProbeResult{
		Interface:  req.Interface,
		Address:    req.Address,
		Status:     IPProbeStatusUnknown,
		Responders: []IPResponder{},
		Warning:    IPProbeWarning,
	}
}

func probeFailure(result IPProbeResult, status int, code, message string) (IPProbeResult, error) {
	result.Status = IPProbeStatusUnknown
	result.Available = nil
	result.Reason = code
	return result, &ProbeError{Status: status, Code: code, Message: message}
}

func probeContextFailure(result IPProbeResult, err error) (IPProbeResult, error) {
	if errors.Is(err, context.DeadlineExceeded) {
		return probeFailure(result, 504, "probe_timeout", "IP probe deadline exceeded")
	}
	return probeFailure(result, 503, "probe_canceled", "IP probe canceled")
}

func finishIPProbe(result IPProbeResult) IPProbeResult {
	available := len(result.Responders) == 0
	result.Available = &available
	if available {
		result.Status = IPProbeStatusNoConflictDetected
	} else {
		result.Status = IPProbeStatusOccupied
	}
	return result
}

type probeInterface struct {
	iface    net.Interface
	prefixes []netip.Prefix
}

type probeNetwork struct {
	iface     net.Interface
	target    netip.Addr
	localMACs map[[6]byte]struct{}
}

func prepareIPProbe(req IPProbeRequest) (probeNetwork, IPProbeResult, error) {
	result := newIPProbeResult(req)
	interfaces, err := net.Interfaces()
	if err != nil {
		result, err = probeFailure(result, 503, "interface_unavailable", "Cannot enumerate local interfaces")
		return probeNetwork{}, result, err
	}
	networks := make([]probeInterface, 0, len(interfaces))
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			result, err = probeFailure(result, 503, "interface_unavailable", "Cannot enumerate local interface addresses")
			return probeNetwork{}, result, err
		}
		network := probeInterface{iface: iface}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err == nil && prefix.Addr().Is4() {
				network.prefixes = append(network.prefixes, prefix)
			}
		}
		networks = append(networks, network)
	}
	return validateIPProbe(req, networks)
}

// validateIPProbe uses only interface inventory, never routing or neighbor caches.
func validateIPProbe(req IPProbeRequest, interfaces []probeInterface) (probeNetwork, IPProbeResult, error) {
	result := newIPProbeResult(req)
	fail := func(code, message string) (probeNetwork, IPProbeResult, error) {
		result, err := probeFailure(result, 400, code, message)
		return probeNetwork{}, result, err
	}
	target, err := netip.ParseAddr(req.Address)
	if err != nil || !target.Is4() || target.IsUnspecified() || target.IsMulticast() || target == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return fail("invalid_address", "Address must be a single unicast IPv4 address")
	}
	selected := -1
	for index := range interfaces {
		if interfaces[index].iface.Name == req.Interface {
			selected = index
			break
		}
	}
	if selected < 0 || req.Interface == "" {
		return fail("invalid_interface", "Interface does not exist")
	}
	iface := interfaces[selected].iface
	if iface.Flags&net.FlagLoopback != 0 || len(iface.HardwareAddr) != 6 || iface.HardwareAddr[0]&1 != 0 || [6]byte(iface.HardwareAddr) == [6]byte{} {
		return fail("invalid_interface", "Interface must be a non-loopback Ethernet interface")
	}
	if iface.Flags&net.FlagUp == 0 {
		result, err := probeFailure(result, 503, "interface_down", "Interface is administratively down")
		return probeNetwork{}, result, err
	}
	network := probeNetwork{iface: iface, target: target, localMACs: make(map[[6]byte]struct{}, len(interfaces))}
	for _, candidate := range interfaces {
		if len(candidate.iface.HardwareAddr) == 6 {
			network.localMACs[[6]byte(candidate.iface.HardwareAddr)] = struct{}{}
		}
		for _, prefix := range candidate.prefixes {
			if prefix.Addr() == target {
				mac := candidate.iface.HardwareAddr.String()
				if len(candidate.iface.HardwareAddr) != 6 {
					mac = ""
				}
				result.Responders = append(result.Responders, IPResponder{MAC: mac, Source: "local_address"})
				result.Reason = "local_address"
				return network, finishIPProbe(result), nil
			}
		}
	}
	boundary := ""
	for _, prefix := range interfaces[selected].prefixes {
		if !prefix.Contains(target) {
			continue
		}
		if prefix.Bits() <= 30 {
			targetBytes := target.As4()
			base := prefix.Masked().Addr().As4()
			if targetBytes == base {
				boundary = "network_address"
				continue
			}
			broadcast := base
			for bit := prefix.Bits(); bit < 32; bit++ {
				broadcast[bit/8] |= 1 << (7 - bit%8)
			}
			if targetBytes == broadcast {
				boundary = "broadcast_address"
				continue
			}
		}
		return network, result, nil
	}
	if boundary != "" {
		return fail(boundary, "Address is a subnet network or broadcast address")
	}
	return fail("off_link_address", "Address is not in an IPv4 prefix directly assigned to the selected interface")
}
