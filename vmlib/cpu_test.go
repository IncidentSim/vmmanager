package vmlib

import (
	"errors"
	"math"
	"testing"
)

func TestParseCPULimitIsExactDecimalMillicores(t *testing.T) {
	ok := map[string]uint32{"0.5": 500, "1": 1000, "1.0": 1000, "0.25": 250, "0.01": 10, "2.5": 2500, "0.50": 500, "64": 64000}
	for in, want := range ok {
		got, err := ParseCPULimit(in)
		if err != nil || got != want {
			t.Errorf("%q: got %d, %v want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "0.0", "-0.5", "+0.5", "NaN", "Inf", "inf", "1e0", "5e-1", "0.005", "0.501", "0.333", ".5", "1.", "0x1", " 0.5", "0.5 ", "65", "64.01", "1000000000000", "0.5.1", "٠.٥"} {
		if v, err := ParseCPULimit(bad); err == nil || !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%q accepted as %d (err=%v)", bad, v, err)
		}
	}
}

func TestCPULimitFromFloatRejectsNonfiniteAndUnrepresentable(t *testing.T) {
	if m, err := CPULimitFromFloat(0.5); err != nil || m != 500 {
		t.Fatalf("%d %v", m, err)
	}
	if m, err := CPULimitFromFloat(0.07); err != nil || m != 70 { // 0.07*1000 is not exact in binary floating point
		t.Fatalf("%d %v", m, err)
	}
	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1), 0.001, 0.505, 0.5000000001, 0.4999999999, 65, 1e300, math.SmallestNonzeroFloat64} {
		if m, err := CPULimitFromFloat(bad); err == nil {
			t.Errorf("%v accepted as %d", bad, m)
		}
	}
}

func TestValidateCPULimitMilli(t *testing.T) {
	for _, m := range []uint32{10, 500, 1000, 64000} {
		if err := ValidateCPULimitMilli(m); err != nil {
			t.Errorf("%d: %v", m, err)
		}
	}
	for _, m := range []uint32{0, 5, 505, 64010, math.MaxUint32} {
		if err := ValidateCPULimitMilli(m); err == nil {
			t.Errorf("%d accepted", m)
		}
	}
}

func TestFormatCPULimitRoundTrips(t *testing.T) {
	for _, m := range []uint32{10, 250, 500, 1000, 1500, 2000, 64000} {
		s := FormatCPULimit(m)
		back, err := ParseCPULimit(s)
		if err != nil || back != m {
			t.Errorf("%d -> %q -> %d, %v", m, s, back, err)
		}
	}
	if FormatCPULimit(500) != "0.5" || FormatCPULimit(1000) != "1" || FormatCPULimit(1250) != "1.25" {
		t.Fatal(FormatCPULimit(500), FormatCPULimit(1000), FormatCPULimit(1250))
	}
}

func TestProfileValidateRequiresConsistentCPUAndMemory(t *testing.T) {
	good := Profile{Name: "small", VCPU: 1, CPULimitMilli: 500, MemMiB: 500, DiskMiB: 2048}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]Profile{
		"no cpu limit":     {Name: "small", VCPU: 1, MemMiB: 500, DiskMiB: 1},
		"limit above vcpu": {Name: "small", VCPU: 1, CPULimitMilli: 1010, MemMiB: 500, DiskMiB: 1},
		"unrepresentable":  {Name: "small", VCPU: 1, CPULimitMilli: 505, MemMiB: 500, DiskMiB: 1},
		"zero vcpu":        {Name: "small", CPULimitMilli: 500, MemMiB: 500, DiskMiB: 1},
		"zero mem":         {Name: "small", VCPU: 1, CPULimitMilli: 500, DiskMiB: 1},
		"bad name":         {Name: "Small!", VCPU: 1, CPULimitMilli: 500, MemMiB: 500, DiskMiB: 1},
		"huge vcpu":        {Name: "small", VCPU: 1 << 20, CPULimitMilli: 500, MemMiB: 500, DiskMiB: 1},
	} {
		if err := p.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestProfileResourcesIsTheImmutableReservation(t *testing.T) {
	p := Profile{Name: "small", VCPU: 1, CPULimitMilli: 500, MemMiB: 500, DiskMiB: 2048}
	r := p.Resources()
	if r != (VMResources{VCPU: 1, CPULimitMilli: 500, MemMiB: 500}) {
		t.Fatalf("%+v", r)
	}
	if r.MemoryMiB() != 500 || r.CPULimit() != 0.5 {
		t.Fatal(r.MemoryMiB(), r.CPULimit())
	}
}

func TestResolveProfileReturnsACopyAndNamesUnknown(t *testing.T) {
	cat := []Profile{{Name: "small", VCPU: 1, CPULimitMilli: 500, MemMiB: 500, DiskMiB: 2048}}
	p, err := ResolveProfile(cat, "small")
	if err != nil || p.MemMiB != 500 {
		t.Fatal(p, err)
	}
	p.MemMiB = 1
	if cat[0].MemMiB != 500 {
		t.Fatal("catalogue aliased")
	}
	if _, err := ResolveProfile(cat, "nope"); !errors.Is(err, ErrUnknownProfile) || !errors.Is(err, ErrInvalidArgument) {
		t.Fatal(err)
	}
}

func TestEffectiveCPULimitFallsBackForLegacyReservations(t *testing.T) {
	if (VMResources{VCPU: 2}).EffectiveCPULimitMilli() != 2000 {
		t.Fatal("legacy reservation must count at the full vCPU allowance")
	}
	if (VMResources{VCPU: 2, CPULimitMilli: 300}).EffectiveCPULimitMilli() != 300 {
		t.Fatal("explicit limit ignored")
	}
}
