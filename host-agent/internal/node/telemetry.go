package node

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/lxc/incus/v6/shared/api"
)

const maxGPUOutputBytes = 1 << 20

// GPUInfo retains inventory identity even when live telemetry is unavailable.
// A nil metric is unknown, not an idle GPU.
type GPUInfo struct {
	UUID                     string   `json:"uuid"`
	PCIAddress               string   `json:"pci_address"`
	Model                    string   `json:"model"`
	UtilizationPercent       *float64 `json:"utilization_percent"`
	MemoryUsedMiB            *float64 `json:"memory_used_mib"`
	MemoryTotalMiB           *float64 `json:"memory_total_mib"`
	MemoryUtilizationPercent *float64 `json:"memory_utilization_percent"`
	LoadPercent              *float64 `json:"load_percent"`
	Error                    string   `json:"error,omitempty"`
}

func collectGPUs(ctx context.Context, resources *api.Resources) ([]GPUInfo, *float64) {
	if resources == nil || len(resources.GPU.Cards) == 0 {
		return []GPUInfo{}, nil
	}
	query := false
	for _, card := range resources.GPU.Cards {
		if card.Nvidia != nil && card.Nvidia.UUID != "" {
			query = true
			break
		}
	}
	if !query {
		return mergeGPUInfo(resources, nil, nil)
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=uuid,name,utilization.gpu,memory.used,memory.total", "--format=csv,noheader,nounits")
	output := &gpuOutputBuffer{}
	command.Stdout = output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return mergeGPUInfo(resources, nil, fmt.Errorf("NVIDIA telemetry unavailable: %w", err))
	}
	return mergeGPUInfo(resources, parseGPUOutput(output.buffer.String()), nil)
}

// Returning a write error also stops os/exec from copying unbounded output.
// The command context bounds the lifetime of a producer that ignores the error.
type gpuOutputBuffer struct {
	buffer bytes.Buffer
}

func (output *gpuOutputBuffer) Write(data []byte) (int, error) {
	remaining := maxGPUOutputBytes - output.buffer.Len()
	if len(data) > remaining {
		n, _ := output.buffer.Write(data[:remaining])
		return n, errors.New("NVIDIA telemetry output exceeds size limit")
	}
	return output.buffer.Write(data)
}

// parseGPUOutput treats each physical output line as one GPU record so a broken
// quoted row cannot consume subsequent GPUs. nvidia-smi GPU names do not contain
// newlines; encoding/csv still handles quoted names containing commas.
func parseGPUOutput(output string) map[string]GPUInfo {
	result := make(map[string]GPUInfo)
	for output != "" {
		line, rest, _ := strings.Cut(output, "\n")
		output = rest
		if strings.TrimSpace(line) == "" {
			continue
		}
		reader := csv.NewReader(strings.NewReader(line))
		reader.TrimLeadingSpace = true
		reader.FieldsPerRecord = -1
		fields, err := reader.Read()
		if len(fields) == 0 {
			continue
		}
		uuid := strings.TrimSpace(fields[0])
		if !gpuUUIDPattern.MatchString(strings.ToUpper(uuid)) {
			continue
		}
		key := strings.ToLower(uuid)
		if previous, exists := result[key]; exists {
			result[key] = GPUInfo{UUID: previous.UUID, Model: previous.Model, Error: "duplicate NVIDIA telemetry UUID"}
			continue
		}
		gpu := GPUInfo{UUID: uuid}
		if len(fields) > 1 {
			gpu.Model = strings.TrimSpace(fields[1])
			if strings.EqualFold(gpu.Model, "N/A") || strings.EqualFold(gpu.Model, "[N/A]") {
				gpu.Model = ""
			}
		}
		if err != nil || len(fields) != 5 {
			gpu.Error = "invalid NVIDIA telemetry CSV record"
		} else if gpu.Model == "" {
			gpu.Error = "NVIDIA telemetry model is unavailable"
		} else {
			utilization, utilErr := parseGPUMetric(fields[2])
			used, usedErr := parseGPUMetric(fields[3])
			total, totalErr := parseGPUMetric(fields[4])
			if utilErr != nil || usedErr != nil || totalErr != nil || utilization > 100 || total <= 0 || used > total {
				gpu.Error = "invalid NVIDIA utilization or memory metrics"
			} else {
				memoryUtilization := used / total * 100
				load := (utilization + memoryUtilization) / 2
				gpu.UtilizationPercent = &utilization
				gpu.MemoryUsedMiB = &used
				gpu.MemoryTotalMiB = &total
				gpu.MemoryUtilizationPercent = &memoryUtilization
				gpu.LoadPercent = &load
			}
		}
		result[key] = gpu
	}
	return result
}

func parseGPUMetric(field string) (float64, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(field), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, errors.New("invalid GPU metric")
	}
	return value, nil
}

// mergeGPUInfo uses only UUIDs to join live samples to the complete Incus
// inventory. Inventory order and identity are retained, including unsupported
// cards; an unknown card makes the whole-node average unknown.
func mergeGPUInfo(resources *api.Resources, samples map[string]GPUInfo, collectionErr error) ([]GPUInfo, *float64) {
	gpus := make([]GPUInfo, 0)
	if resources == nil {
		return gpus, nil
	}
	gpus = make([]GPUInfo, 0, len(resources.GPU.Cards))
	counts := make(map[string]int, len(resources.GPU.Cards))
	for _, card := range resources.GPU.Cards {
		if card.Nvidia != nil && card.Nvidia.UUID != "" {
			counts[strings.ToLower(card.Nvidia.UUID)]++
		}
	}
	var sum float64
	complete := len(resources.GPU.Cards) > 0
	for _, card := range resources.GPU.Cards {
		gpu := GPUInfo{PCIAddress: card.PCIAddress, Model: card.Product}
		if card.Nvidia == nil {
			gpu.Error = "live GPU metrics are unavailable for non-NVIDIA cards"
		} else {
			gpu.UUID = card.Nvidia.UUID
			if card.Nvidia.Model != "" {
				gpu.Model = card.Nvidia.Model
			}
			key := strings.ToLower(gpu.UUID)
			sample, found := samples[key]
			if sample.Model != "" {
				gpu.Model = sample.Model
			}
			switch {
			case !gpuUUIDPattern.MatchString(strings.ToUpper(gpu.UUID)):
				gpu.Error = "Incus does not advertise a valid NVIDIA GPU UUID"
			case counts[key] != 1:
				gpu.Error = "duplicate NVIDIA GPU UUID in Incus inventory"
			case collectionErr != nil:
				gpu.Error = collectionErr.Error()
			case !found:
				gpu.Error = "NVIDIA telemetry is missing for GPU UUID"
			case sample.Error != "":
				gpu.Error = sample.Error
			default:
				gpu.UtilizationPercent = sample.UtilizationPercent
				gpu.MemoryUsedMiB = sample.MemoryUsedMiB
				gpu.MemoryTotalMiB = sample.MemoryTotalMiB
				gpu.MemoryUtilizationPercent = sample.MemoryUtilizationPercent
				gpu.LoadPercent = sample.LoadPercent
			}
		}
		if gpu.LoadPercent == nil {
			complete = false
		} else {
			sum += *gpu.LoadPercent
		}
		gpus = append(gpus, gpu)
	}
	if !complete {
		return gpus, nil
	}
	average := sum / float64(len(gpus))
	return gpus, &average
}
