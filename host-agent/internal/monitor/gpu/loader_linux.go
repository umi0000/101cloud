//go:build linux && (amd64 || arm64)

package gpu

import (
	"errors"

	"github.com/ebitengine/purego"
)

// openBackend is called only while the process-wide native slot is held.
// NVML's init/shutdown pair is reference counted, so independent readers retain
// independent library references without shutting down another active reader.
func openBackend() (backend, error) {
	lib, err := purego.Dlopen("libnvidia-ml.so.1", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, ErrUnavailable
	}
	bind := func(target any, symbol string) bool {
		address, err := purego.Dlsym(lib, symbol)
		if err != nil || address == 0 {
			return false
		}
		purego.RegisterFunc(target, address)
		return true
	}
	b := &nvmlBackend{}
	var init func() int32
	if !bind(&init, "nvmlInit_v2") ||
		!bind(&b.shutdown, "nvmlShutdown") ||
		!bind(&b.count, "nvmlDeviceGetCount_v2") ||
		!bind(&b.device, "nvmlDeviceGetHandleByIndex_v2") ||
		!bind(&b.uuid, "nvmlDeviceGetUUID") {
		_ = purego.Dlclose(lib)
		return nil, ErrUnavailable
	}
	bind(&b.compute, "nvmlDeviceGetComputeRunningProcesses_v2")
	bind(&b.graphics, "nvmlDeviceGetGraphicsRunningProcesses_v2")
	bind(&b.mps, "nvmlDeviceGetMPSComputeRunningProcesses_v2")
	bind(&b.utilization, "nvmlDeviceGetProcessUtilization")
	bind(&b.mig, "nvmlDeviceGetMigMode")
	if code := init(); code != nvSuccess {
		_ = purego.Dlclose(lib)
		return nil, errors.New("NVML initialization " + status(code))
	}
	b.unload = func() error {
		if purego.Dlclose(lib) != nil {
			return errors.New("NVML library release failed")
		}
		return nil
	}
	return b, nil
}
