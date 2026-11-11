package vmlib

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	// CPUMilli is the integer unit of CPU time: 1000 = one full CPU.
	CPUMilli = 1000
	// CPUQuotaStepMilli is the granularity of the physical limit. systemd's
	// CPUQuota= is a whole percent, so only multiples of 10 millicores are
	// representable; anything else is rejected rather than silently rounded.
	CPUQuotaStepMilli = 10
	// MaxCPULimitMilli bounds a limit at 64 CPUs (the backend's slot ceiling).
	MaxCPULimitMilli = 64 * CPUMilli
	// MaxVCPU bounds a guest vCPU count (Firecracker's own ceiling is 32).
	MaxVCPU = 32
)

// ErrUnknownProfile wraps ErrInvalidArgument for an unconfigured profile name.
var ErrUnknownProfile = fmt.Errorf("%w: unknown profile", ErrInvalidArgument)

// ValidateCPULimitMilli accepts a positive multiple of CPUQuotaStepMilli that
// is at most MaxCPULimitMilli.
func ValidateCPULimitMilli(m uint32) error {
	if m == 0 || m > MaxCPULimitMilli || m%CPUQuotaStepMilli != 0 {
		return fmt.Errorf("%w: cpu_limit %d millicores must be a positive multiple of %d and at most %d", ErrInvalidArgument, m, CPUQuotaStepMilli, MaxCPULimitMilli)
	}
	return nil
}

// ParseCPULimit parses an exact plain decimal CPU count ("0.5", "1", "2.25")
// into millicores. No sign, exponent, NaN/Inf, whitespace or more than two
// decimals; values not representable as a whole-percent quota are rejected.
func ParseCPULimit(s string) (uint32, error) {
	bad := func(why string) (uint32, error) {
		return 0, fmt.Errorf("%w: cpu_limit %q %s", ErrInvalidArgument, truncate(s, 24), why)
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || len(whole) > 3 || (hasDot && (frac == "" || len(frac) > 2)) {
		return bad("must be a plain decimal such as 0.5 with at most two decimals")
	}
	for _, part := range []string{whole, frac} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return bad("must be a plain decimal such as 0.5 with at most two decimals")
			}
		}
	}
	w, err := strconv.Atoi(whole)
	if err != nil {
		return bad("is not a number")
	}
	f := 0
	if hasDot {
		f, _ = strconv.Atoi(frac + strings.Repeat("0", 2-len(frac)))
	}
	m := uint64(w)*CPUMilli + uint64(f)*10
	if m > math.MaxUint32 {
		return bad("is out of range")
	}
	if err := ValidateCPULimitMilli(uint32(m)); err != nil {
		return 0, err
	}
	return uint32(m), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// CPULimitFromFloat converts a CPU count to millicores for callers that hold a
// float (e.g. a decoded number). It rejects NaN, infinities, non-positive and
// unrepresentable values; the result is exact, never rounded to fit.
func CPULimitFromFloat(f float64) (uint32, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > MaxCPULimitMilli/CPUMilli {
		return 0, fmt.Errorf("%w: cpu_limit must be a finite number of CPUs in (0, %d]", ErrInvalidArgument, MaxCPULimitMilli/CPUMilli)
	}
	m := math.Round(f * CPUMilli)
	if f != m/CPUMilli {
		return 0, fmt.Errorf("%w: cpu_limit is not a whole number of millicores", ErrInvalidArgument)
	}
	if err := ValidateCPULimitMilli(uint32(m)); err != nil {
		return 0, err
	}
	return uint32(m), nil
}

// FormatCPULimit renders millicores as the shortest exact decimal ("0.5").
func FormatCPULimit(m uint32) string {
	s := fmt.Sprintf("%d.%03d", m/CPUMilli, m%CPUMilli)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// Validate checks a catalogue entry in isolation: identity, positive resources,
// a representable CPU limit that does not exceed the guest vCPU count.
func (p Profile) Validate() error {
	if err := ValidateID("profile", p.Name); err != nil {
		return err
	}
	if p.VCPU == 0 || p.VCPU > MaxVCPU || p.MemMiB == 0 {
		return fmt.Errorf("%w: profile %s needs vcpu 1..%d and memory_mib > 0", ErrInvalidArgument, p.Name, MaxVCPU)
	}
	if err := ValidateCPULimitMilli(p.CPULimitMilli); err != nil {
		return fmt.Errorf("profile %s: %w", p.Name, err)
	}
	if p.CPULimitMilli > p.VCPU*CPUMilli {
		return fmt.Errorf("%w: profile %s cpu_limit %s exceeds its %d guest vCPU(s)", ErrInvalidArgument, p.Name, FormatCPULimit(p.CPULimitMilli), p.VCPU)
	}
	return nil
}

// Resources is the reservation a VM created from this profile receives. It is
// a value: later edits to the catalogue never change a VM that stored it.
func (p Profile) Resources() VMResources {
	return VMResources{VCPU: p.VCPU, CPULimitMilli: p.CPULimitMilli, MemMiB: p.MemMiB}
}

// ResolveProfile looks a name up in the (single) backend catalogue and returns
// a copy. It is the seam the API layer uses; there is no second profile list.
func ResolveProfile(catalogue []Profile, name string) (Profile, error) {
	if err := ValidateID("profile", name); err != nil {
		return Profile{}, err
	}
	for _, p := range catalogue {
		if p.Name == name {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("%w %q", ErrUnknownProfile, name)
}

// MemoryMiB is the public (memory_mib) name of the reserved guest memory.
func (r VMResources) MemoryMiB() uint32 { return r.MemMiB }

// CPULimit is the limit in CPUs for display only; never persist or sum it.
func (r VMResources) CPULimit() float64 { return float64(r.CPULimitMilli) / CPUMilli }

// EffectiveCPULimitMilli is the limit this reservation is counted and
// enforced at: its own, or for a pre-limit reservation the full guest vCPU
// allowance (the most it could always use).
func (r VMResources) EffectiveCPULimitMilli() uint32 {
	if r.CPULimitMilli != 0 {
		return r.CPULimitMilli
	}
	return r.VCPU * CPUMilli
}
