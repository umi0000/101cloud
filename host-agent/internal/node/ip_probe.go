package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"
	"unicode"
)

const (
	maxIPProbeBody        = 4 << 10
	maxConcurrentIPProbes = 4
	ipProbeTimeout        = 5 * time.Second
)

// decodeIPProbeRequest deliberately accepts only one object with two required
// string fields. Reading the bounded body first also makes oversized requests
// consistently return 413, even when their JSON would otherwise be invalid.
func decodeIPProbeRequest(w http.ResponseWriter, r *http.Request) (IPProbeRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxIPProbeBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return IPProbeRequest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil {
		return IPProbeRequest{}, err
	}
	if token != json.Delim('{') {
		return IPProbeRequest{}, errors.New("request body must be one JSON object")
	}
	var request IPProbeRequest
	var seenInterface, seenAddress bool
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return IPProbeRequest{}, err
		}
		key, ok := token.(string)
		if !ok {
			return IPProbeRequest{}, errors.New("JSON field names must be strings")
		}
		var destination *string
		switch key {
		case "interface":
			if seenInterface {
				return IPProbeRequest{}, errors.New("duplicate JSON field interface")
			}
			seenInterface = true
			destination = &request.Interface
		case "address":
			if seenAddress {
				return IPProbeRequest{}, errors.New("duplicate JSON field address")
			}
			seenAddress = true
			destination = &request.Address
		default:
			return IPProbeRequest{}, fmt.Errorf("unknown JSON field %q", key)
		}
		var value *string
		if err := decoder.Decode(&value); err != nil {
			return IPProbeRequest{}, err
		}
		if value == nil {
			return IPProbeRequest{}, fmt.Errorf("%s must be a string, not null", key)
		}
		*destination = *value
	}
	if _, err := decoder.Token(); err != nil {
		return IPProbeRequest{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return IPProbeRequest{}, errors.New("only one JSON object is permitted")
		}
		return IPProbeRequest{}, err
	}
	if !seenInterface || !seenAddress {
		return IPProbeRequest{}, errors.New("interface and address are required")
	}
	if request.Interface == "" || len(request.Interface) > 15 || strings.ContainsAny(request.Interface, "/\\:\x00") || strings.ContainsFunc(request.Interface, unicode.IsSpace) || request.Interface == "." || request.Interface == ".." {
		return IPProbeRequest{}, errors.New("interface must be one non-empty Linux interface name")
	}
	address, err := netip.ParseAddr(request.Address)
	if err != nil || !address.Is4() {
		return IPProbeRequest{}, errors.New("address must be one IPv4 address, not a hostname, range, or CIDR")
	}
	return request, nil
}

func (s *Service) probeIP(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeError(w, http.StatusBadRequest, "invalid_query", "IP probing does not accept query parameters")
		return
	}
	request, err := decodeIPProbeRequest(w, r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "IP probe request must not exceed 4 KiB")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ipProbeTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		writeIPProbeFailure(w, request, err)
		return
	}
	// Slots are never queued: callers can retry later instead of holding an
	// unbounded set of requests while privileged packet sockets are occupied.
	select {
	case s.ipProbeSlots <- struct{}{}:
		defer func() { <-s.ipProbeSlots }()
	default:
		writeIPProbeFailure(w, request, &ProbeError{Status: http.StatusServiceUnavailable, Code: "probe_busy", Message: "all four IP probe slots are in use"})
		return
	}
	result, err := s.ipProber(ctx, request)
	// Cancellation overrides even an apparently successful observation: a
	// partial probe window is not evidence that the address is conflict-free.
	if contextErr := ctx.Err(); contextErr != nil {
		err = contextErr
	}
	if err != nil {
		writeIPProbeFailure(w, request, err)
		return
	}
	result.Interface = request.Interface
	result.Address = request.Address
	result.Warning = IPProbeWarning
	available := false
	switch result.Status {
	case IPProbeStatusOccupied:
	case IPProbeStatusNoConflictDetected:
		available = true
	default:
		writeIPProbeFailure(w, request, &ProbeError{Status: http.StatusServiceUnavailable, Code: "probe_unknown", Message: "the probe did not produce a conclusive observation"})
		return
	}
	result.Available = &available
	if result.Responders == nil {
		result.Responders = []IPResponder{}
	}
	writeJSON(w, http.StatusOK, result)
}

func writeIPProbeFailure(w http.ResponseWriter, request IPProbeRequest, err error) {
	status, code := http.StatusServiceUnavailable, "probe_failed"
	var probeErr *ProbeError
	if errors.As(err, &probeErr) {
		code = probeErr.Code
		switch probeErr.Status {
		case http.StatusBadRequest, http.StatusNotImplemented, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			status = probeErr.Status
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		status, code = http.StatusGatewayTimeout, "probe_timeout"
	} else if errors.Is(err, context.Canceled) {
		status, code = http.StatusServiceUnavailable, "probe_canceled"
	}
	writeJSON(w, status, struct {
		IPProbeResult
		Error   string `json:"error"`
		Message string `json:"message"`
	}{
		IPProbeResult: IPProbeResult{
			Interface:  request.Interface,
			Address:    request.Address,
			Status:     IPProbeStatusUnknown,
			Available:  nil,
			Responders: []IPResponder{},
			Reason:     code,
			Warning:    IPProbeWarning,
		},
		Error:   code,
		Message: err.Error(),
	})
}
