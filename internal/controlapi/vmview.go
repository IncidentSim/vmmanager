package controlapi

import "github.com/petruekhin/vibe-firecracker/internal/vmbot"

// The durable domain accounts CPU in integer millicores. Only this HTTP
// projection converts it to CPU units; no persisted arithmetic uses floats.
type publicResources struct {
	VCPU      uint32  `json:"vcpu"`
	CPULimit  float64 `json:"cpu_limit"`
	MemoryMiB uint32  `json:"memory_mib"`
}
type publicVM struct {
	vmbot.ControlVMView
	Resources *publicResources `json:"resources"`
}

func projectVM(v vmbot.ControlVMView) publicVM {
	out := publicVM{ControlVMView: v}
	if r := v.Resources; r != nil {
		milli := r.CPULimitMilli
		if milli == 0 {
			milli = r.VCPU * 1000
		}
		out.Resources = &publicResources{VCPU: r.VCPU, CPULimit: float64(milli) / 1000, MemoryMiB: r.MemoryMiB}
	}
	return out
}
func projectVMs(vs []vmbot.ControlVMView) []publicVM {
	out := make([]publicVM, 0, len(vs))
	for _, v := range vs {
		out = append(out, projectVM(v))
	}
	return out
}
