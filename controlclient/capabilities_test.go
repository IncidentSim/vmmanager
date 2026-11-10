package controlclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCapabilitiesDecodesBackendProfilesAndAggregateCaps(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"max_vms":2,"max_vms_per_user":1,"backend_max_vms":9,"max_total_memory_mib":6144,"max_total_vcpu":6,` +
			`"profiles":[{"name":"wide","vcpu":4,"memory_mib":3072,"disk_mib":8192}]}`))
	}))
	defer s.Close()
	c, _ := New(s.URL, "token", nil)
	v, e := c.Capabilities(context.Background(), 42)
	if e != nil {
		t.Fatal(e)
	}
	if v.MaxVMs != 2 || v.MaxVMsPerUser != 1 || v.BackendMaxVMs != 9 || v.MaxTotalMemoryMiB != 6144 || v.MaxTotalVCPU != 6 {
		t.Fatalf("%+v", v)
	}
	if len(v.Profiles) != 1 || v.Profiles[0] != (Profile{Name: "wide", VCPU: 4, MemoryMiB: 3072, DiskMiB: 8192}) {
		t.Fatalf("%+v", v.Profiles)
	}
}
