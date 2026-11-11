package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func cpuMaxRatio(data string) (float64, error) {
	fields := strings.Fields(data)
	if len(fields) != 2 || fields[0] == "max" {
		return 0, fmt.Errorf("missing finite cgroup v2 CPU quota")
	}
	quota, e := strconv.ParseUint(fields[0], 10, 64)
	if e != nil || quota == 0 {
		return 0, fmt.Errorf("invalid CPU quota")
	}
	period, e := strconv.ParseUint(fields[1], 10, 64)
	if e != nil || period == 0 {
		return 0, fmt.Errorf("invalid CPU period")
	}
	return float64(quota) / float64(period), nil
}

// No privileged command or sudo is run. PID association is supplied by an
// explicitly trusted, deployment-specific read-only resolver; cpu.max is read
// independently from the kernel, never accepted from API metadata/script JSON.
func TestOptionalPhysicalCPUReadback(t *testing.T) {
	s := newSuite(t)
	if os.Getenv("VMAPI_E2E_HOST_READBACK") != "1" {
		t.Skip("physical CPU evidence requires separate HOST_READBACK=1 and trusted local VM-to-VMM PID resolver; API JSON is NOT cgroup proof")
	}
	resolver := os.Getenv("VMAPI_E2E_VMM_PID_RESOLVER")
	if !filepath.IsAbs(resolver) {
		t.Fatal("VMM_PID_RESOLVER must be explicit absolute executable path")
	}
	st, e := os.Stat(resolver)
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 || st.Mode().Perm()&0022 != 0 {
		t.Fatal("VMM_PID_RESOLVER must be executable regular file not writable by group/world")
	}
	s.preflight()
	a := s.createActive("cpu", s.cfg.BaseImageID)
	vm, e := s.c.VM(s.ctx, s.owner, a.VMID)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	out, e := exec.CommandContext(ctx, resolver, a.VMID).Output()
	if e != nil {
		t.Fatal("trusted VMM PID resolver failed; physical CPU NOT proven")
	}
	pid, e := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 32)
	if e != nil || pid <= 1 {
		t.Fatal("resolver did not return a single positive VMM PID")
	}
	proc := filepath.Join("/proc", strconv.FormatInt(pid, 10))
	exe, e := os.Readlink(filepath.Join(proc, "exe"))
	if e != nil || !strings.HasPrefix(filepath.Base(exe), "firecracker") {
		t.Fatal("resolved PID is not an inspectable Firecracker process")
	}
	cg, e := os.ReadFile(filepath.Join(proc, "cgroup"))
	if e != nil {
		t.Fatal("cannot read VMM process cgroup")
	}
	path := ""
	for _, line := range strings.Split(string(cg), "\n") {
		if strings.HasPrefix(line, "0::/") {
			path = strings.TrimPrefix(line, "0::")
		}
	}
	if path == "" || path == "/" || strings.Contains(path, "..") {
		t.Fatal("per-VM cgroup v2 transport unavailable; physical CPU NOT proven")
	}
	quota, e := os.ReadFile(filepath.Join("/sys/fs/cgroup", path, "cpu.max"))
	if e != nil {
		t.Fatal("cannot read actual VMM cgroup cpu.max; physical CPU NOT proven")
	}
	procs, e := os.ReadFile(filepath.Join("/sys/fs/cgroup", path, "cgroup.procs"))
	if e != nil || strings.TrimSpace(string(procs)) != strconv.FormatInt(pid, 10) {
		t.Fatal("resolved cgroup is not isolated to the single VMM PID")
	}
	ratio, e := cpuMaxRatio(string(quota))
	if e != nil || ratio != vm.Resources.CPULimit {
		t.Fatal("actual VMM cgroup CPU quota does not equal effective reservation")
	}
	again, e := os.ReadFile(filepath.Join(proc, "cgroup"))
	if e != nil || string(again) != string(cg) {
		t.Fatal("VMM cgroup changed during readback")
	}
	t.Log("kernel cpu.max matches CPU reservation for operator-resolved Firecracker PID (mapping trust: explicit local resolver); not a throughput benchmark")
}

func TestOptionalPerUserQuotaAdmission(t *testing.T) {
	s := newSuite(t)
	if os.Getenv("VMAPI_E2E_QUOTA") != "1" {
		t.Skip("quota saturation separately requires QUOTA=1; creates up to four owned VMs")
	}
	s.preflight()
	limit := s.caps.MaxVMsPerUser
	if limit < 1 || limit > 4 {
		t.Skip("quota fixture bounded to configured per-user limit 1..4")
	}
	remaining := limit - len(s.tr.preVMs)
	if s.caps.BackendMaxVMs > 0 && remaining > s.caps.BackendMaxVMs {
		t.Skip("backend slots cannot demonstrate per-user quota; no capacity is inferred")
	}
	for i := 0; i < remaining; i++ {
		a := s.admitRequest(s.request(fmt.Sprintf("q%d", i), s.cfg.BaseImageID))
		s.mustWait("quota create", a.OperationID)
	}
	a, e := s.c.Create(s.ctx, s.owner, s.n.key("over-quota"), s.request("qextra", s.cfg.BaseImageID))
	s.trackAccepted(acceptedCreate, a)
	if !isStatus(e, 429) {
		t.Fatalf("over-quota admission: %v, want HTTP 429", e)
	}
	vms, e := s.c.ListVMs(s.ctx, s.owner)
	if e != nil || len(vms) != limit {
		t.Fatal("over-quota request changed visible reservation count")
	}
	t.Log("per-user limit admission refused at configured count; global/other-user capacity isolation not proven")
}
