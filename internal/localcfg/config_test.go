package localcfg

import (
	"strings"
	"testing"
)

const good = `{"work_root":"/w","images_dir":"/i","run_dir":"/r","max_vms":2,"helper":"/usr/local/libexec/fc-vm-helper","ssh_user":"root","storage_mount":"/var/lib/firecracker-builder/storage","max_total_mem_mib":1024,"max_total_vcpu":1,"unit_mem_limit_mib":768,"guest_prefix":"10.213","pool_conf":"/etc/firecracker-vms/pool.conf","profiles":[{"name":"small","vcpu":1,"mem_mib":256,"disk_mib":1024}]}`

func TestParseRejectsUnsafeShapes(t *testing.T) {
	cases := map[string]struct {
		json string
		ok   bool
	}{
		"valid":              {good, true},
		"no storage mount":   {strings.Replace(good, `"storage_mount":"/var/lib/firecracker-builder/storage",`, "", 1), false},
		"no aggregate caps":  {strings.Replace(good, `"max_total_mem_mib":1024,`, "", 1), false},
		"no guest prefix":    {strings.Replace(good, `"guest_prefix":"10.213",`, "", 1), false},
		"no pool conf":       {strings.Replace(good, `"pool_conf":"/etc/firecracker-vms/pool.conf",`, "", 1), false},
		"relative helper":    {strings.Replace(good, "/usr/local/libexec/fc-vm-helper", "helper", 1), false},
		"no helper":          {strings.Replace(good, `"helper":"/usr/local/libexec/fc-vm-helper",`, "", 1), false},
		"no roots":           {strings.Replace(good, `"work_root":"/w",`, "", 1), false},
		"unknown field":      {strings.Replace(good, `"max_vms":2`, `"max_vms":2,"insecure":true`, 1), false},
		"test seam via json": {strings.Replace(good, `"max_vms":2`, `"max_vms":2,"trusted_owner_uid":1000`, 1), false},
		"trailing data":      {good + `{}`, false},
	}
	for name, c := range cases {
		_, err := Parse([]byte(c.json))
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v want ok=%v", name, err, c.ok)
		}
	}
}
