package localcfg

import (
	"strings"
	"testing"
)

func TestFractionalCPUConfigAndLegacyMemory(t *testing.T) {
	for _, memory := range []string{"mem_mib", "memory_mib"} {
		raw := strings.Replace(good, `"vcpu":1`, `"vcpu":1,"cpu_limit":0.5`, 1)
		raw = strings.Replace(raw, `"mem_mib"`, `"`+memory+`"`, 1)
		raw = strings.Replace(raw, `"max_vms":2`, `"max_vms":2,"default_profile":"small","max_total_cpu_limit":1.5`, 1)
		c, err := Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		c.PoolConf = publicationPool(t, "")
		cfg, err := c.Assemble(Seams{Launcher: fakeLauncher{}, VMM: fakeVMM{}})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DefaultProfile != "small" || cfg.MaxTotalCPULimitMilli != 1500 || cfg.Profiles[0].CPULimitMilli != 500 || cfg.Profiles[0].MemMiB != 256 {
			t.Fatalf("lost policy: %+v", cfg)
		}
	}
}

func TestInvalidCPUConfigRejected(t *testing.T) {
	for _, value := range []string{`0`, `-1`, `0.505`, `0.5000000001`, `1e0`, `"0.5"`, `null`, `2`, `65`} {
		raw := strings.Replace(good, `"vcpu":1`, `"vcpu":1,"cpu_limit":`+value, 1)
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("accepted %s", value)
		}
	}
	raw := strings.Replace(good, `"mem_mib":256`, `"mem_mib":256,"memory_mib":256`, 1)
	if _, err := Parse([]byte(raw)); err == nil {
		t.Fatal("ambiguous memory aliases accepted")
	}
}
