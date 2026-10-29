//go:build linux

package node

import (
	"fmt"
	"github.com/godbus/dbus/v5"
)

// D-Bus arrays of structs decode to tuple slices, not necessarily slices of Go
// structs. Decode each tuple explicitly; never flatten away the interface index.
func resolvedTuples(value any) ([][]interface{}, error) {
	switch rows := value.(type) {
	case [][]interface{}:
		return rows, nil
	case []interface{}:
		result := make([][]interface{}, len(rows))
		for i, row := range rows {
			var ok bool
			result[i], ok = row.([]interface{})
			if !ok {
				return nil, fmt.Errorf("invalid resolved tuple")
			}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("invalid resolved tuple array")
	}
}
func decodeResolvedServers(value any) ([]resolvedDNSServer, error) {
	rows, err := resolvedTuples(value)
	if err != nil {
		return nil, err
	}
	result := make([]resolvedDNSServer, len(rows))
	for i, row := range rows {
		if len(row) != 3 {
			return nil, fmt.Errorf("invalid resolved DNS tuple width")
		}
		if err := dbus.Store(row, &result[i].Index, &result[i].Family, &result[i].Address); err != nil {
			return nil, err
		}
	}
	return result, nil
}
func decodeResolvedDomains(value any) ([]resolvedDNSDomain, error) {
	rows, err := resolvedTuples(value)
	if err != nil {
		return nil, err
	}
	result := make([]resolvedDNSDomain, len(rows))
	for i, row := range rows {
		if len(row) != 3 {
			return nil, fmt.Errorf("invalid resolved domain tuple width")
		}
		if err := dbus.Store(row, &result[i].Index, &result[i].Domain, &result[i].RouteOnly); err != nil {
			return nil, err
		}
	}
	return result, nil
}
