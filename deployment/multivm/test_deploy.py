import json
import re
import subprocess
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
NET = (HERE / "fc-vm-network.sh").read_text()


def unit(name):
    """Parse a unit file into {section: [(key, value), ...]} preserving order."""
    out, section = {}, None
    for raw in (HERE / name).read_text().splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("["):
            section = line.strip("[]")
            out[section] = []
        else:
            k, _, v = line.partition("=")
            out[section].append((k, v))
    return out


def values(u, section, key):
    return [v for k, v in u.get(section, []) if k == key]


class NetworkScriptTests(unittest.TestCase):
    """Run `check` unprivileged against canned `ip` output."""

    def run_check(self, slot, routes, addrs="", pool="GUEST_PREFIX=10.213\nMAX_SLOTS=2\n", action="check"):
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            (d / "bin").mkdir()
            (d / "pool.conf").write_text(pool)
            calls = d / "calls"
            fake = d / "bin" / "ip"
            fake.write_text(
                "#!/bin/sh\n"
                f'echo "$@" >> {calls}\n'
                'case "$*" in\n'
                f"  *route*) cat <<'EOF'\n{routes}\nEOF\n;;\n"
                f"  *addr*) cat <<'EOF'\n{addrs}\nEOF\n;;\n"
                "esac\n"
            )
            fake.chmod(0o700)
            script = NET.replace("/etc/firecracker-vms/pool.conf", str(d / "pool.conf"))
            script = script.replace("PATH=/usr/sbin:/usr/bin:/sbin:/bin", f"PATH={d / 'bin'}:/usr/bin:/bin")
            self.assertNotEqual(script, NET)
            path = d / "net.sh"
            path.write_text(script)
            path.chmod(0o700)
            proc = subprocess.run(["bash", str(path), action, str(slot)], capture_output=True, text=True)
            made = calls.read_text() if calls.exists() else ""
            return proc.returncode, proc.stderr, made

    CLEAN = (
        "default via 192.168.1.1 dev eth0 proto dhcp\n"
        "192.168.1.0/24 dev eth0 proto kernel scope link src 192.168.1.50\n"
        "172.16.0.0/30 dev tap-fcpg proto kernel scope link src 172.16.0.1\n"
        "172.17.0.0/16 dev docker0 proto kernel scope link src 172.17.0.1 linkdown"
    )

    def test_unrelated_routes_and_legacy_subnet_are_not_collisions(self):
        rc, err, _ = self.run_check(0, self.CLEAN)
        self.assertEqual(rc, 0, err)

    def test_covering_docker_route_blocks_a_172_17_pool(self):
        rc, err, _ = self.run_check(0, self.CLEAN, pool="GUEST_PREFIX=172.17\nMAX_SLOTS=2\n")
        self.assertNotEqual(rc, 0)
        self.assertIn("overlaps", err)

    def test_covering_and_more_specific_routes_are_collisions(self):
        cases = {
            "covering /8": "10.0.0.0/8 via 192.168.1.1 dev eth0",
            "covering /16": "10.213.0.0/16 dev tun0",
            "exact /30": "10.213.0.0/30 dev x",
            "host route inside": "10.213.0.2 dev x scope link",
            "/31 inside": "10.213.0.2/31 dev x",
            "typed route": "unreachable 10.213.0.0/24",
        }
        for name, route in cases.items():
            rc, err, _ = self.run_check(0, "default via 1.1.1.1 dev eth0\n" + route)
            self.assertNotEqual(rc, 0, name)

    def test_other_slots_network_is_not_a_collision_for_this_slot(self):
        routes = "default via 1.1.1.1 dev eth0\n10.213.1.0/30 dev tap-vm1 proto kernel"
        self.assertEqual(self.run_check(0, routes)[0], 0)
        self.assertNotEqual(self.run_check(1, routes)[0], 0)

    def test_default_route_is_expected(self):
        self.assertEqual(self.run_check(0, "default via 10.213.0.1 dev eth0\n0.0.0.0/0 dev eth0")[0], 0)

    def test_overlapping_interface_address_is_a_collision(self):
        addrs = "2: eth1    inet 10.213.0.9/24 brd 10.213.0.255 scope global eth1"
        rc, err, _ = self.run_check(0, "default via 1.1.1.1 dev eth0", addrs=addrs)
        self.assertNotEqual(rc, 0)
        self.assertIn("overlaps", err)

    def test_bad_pool_and_slots_are_rejected_without_touching_the_network(self):
        for pool in ("GUEST_PREFIX=10.213;reboot\nMAX_SLOTS=2\n", "GUEST_PREFIX=127.0\nMAX_SLOTS=2\n",
                     "GUEST_PREFIX=10.213\nMAX_SLOTS=65\n", "GUEST_PREFIX=10.213\n", "FOO=bar\nGUEST_PREFIX=10.213\nMAX_SLOTS=2\n",
                     "GUEST_PREFIX=10.213\nGUEST_PREFIX=10.214\nMAX_SLOTS=2\n"):
            rc, _, calls = self.run_check(0, self.CLEAN, pool=pool)
            self.assertNotEqual(rc, 0, pool)
            self.assertEqual(calls, "", pool)
        for slot in ("2", "-1", "01", "x", "1;reboot", ""):
            rc, _, calls = self.run_check(slot, self.CLEAN)
            self.assertNotEqual(rc, 0, slot)
            self.assertEqual(calls, "", slot)

    def test_up_and_down_require_root_and_change_nothing(self):
        for action in ("up", "down"):
            rc, err, calls = self.run_check(0, self.CLEAN, action=action)
            self.assertNotEqual(rc, 0)
            self.assertIn("root", err)
            self.assertNotIn("tuntap", calls)

    def test_script_only_touches_its_own_interface_and_tagged_rules(self):
        code = "\n".join(l for l in NET.splitlines() if not l.strip().startswith("#"))
        self.assertNotIn("tap-fcpg", code)
        self.assertNotIn("172.16.", code)
        self.assertNotRegex(code, r"ip route (add|del|replace)")
        self.assertNotRegex(code, r"iptables\s+-F|iptables\s+-X|--flush")


class UnitContractTests(unittest.TestCase):
    STORAGE = "/var/lib/firecracker-builder/storage"

    def test_vm_unit_has_no_network_exec_inside_the_jail(self):
        u = unit("firecracker-vm@.service")
        self.assertEqual(values(u, "Service", "ExecStartPre"), [])
        for key in ("ExecStart", "ExecStopPost", "ExecStartPre", "ExecStop"):
            for v in values(u, "Service", key):
                self.assertNotIn("fc-vm-network", v)
                self.assertFalse(v.startswith(("+", "!", "@", "-")), v)
        self.assertEqual(values(u, "Service", "RootDirectory"), ["/var/lib/firecracker-vms/jail"])

    def test_vm_unit_depends_on_per_slot_network_and_orders_after_it(self):
        u = unit("firecracker-vm@.service")
        self.assertIn("firecracker-vm-network@%i.service", values(u, "Unit", "Requires"))
        self.assertIn("firecracker-vm-network@%i.service", values(u, "Unit", "After"))

    def test_vm_unit_fails_closed_without_the_bounded_storage_mount(self):
        u = unit("firecracker-vm@.service")
        self.assertIn(self.STORAGE, values(u, "Unit", "RequiresMountsFor"))
        self.assertIn(self.STORAGE, values(u, "Unit", "AssertPathIsMountPoint"))
        self.assertEqual(values(u, "Unit", "ConditionPathIsMountPoint"), [])
        self.assertIn("/dev/kvm", values(u, "Unit", "AssertPathExists"))
        self.assertEqual(values(u, "Unit", "ConditionPathExists"), [])
        binds = values(u, "Service", "BindPaths")
        self.assertIn(f"{self.STORAGE}/vms/work:/work", binds)

    def test_vm_unit_is_one_shot_per_guest_and_shares_the_legacy_budget_slice(self):
        u = unit("firecracker-vm@.service")
        self.assertEqual(values(u, "Service", "Restart"), ["no"])
        self.assertEqual(values(u, "Service", "Slice"), [r"system-firecracker\x2dbuilder.slice"])
        self.assertEqual(values(u, "Service", "MemoryMax"), ["768M"])

    def test_network_unit_is_a_host_root_oneshot_with_up_and_down(self):
        u = unit("firecracker-vm-network@.service")
        self.assertEqual(values(u, "Service", "Type"), ["oneshot"])
        self.assertEqual(values(u, "Service", "RemainAfterExit"), ["yes"])
        self.assertEqual(values(u, "Service", "RootDirectory"), [])
        self.assertEqual(values(u, "Service", "ExecStart"), ["/usr/local/libexec/fc-vm-network up %i"])
        self.assertEqual(values(u, "Service", "ExecStop"), ["/usr/local/libexec/fc-vm-network down %i"])

    def test_privilege_boundary_is_a_group_restricted_socket_not_sudo(self):
        s = unit("fc-vm-helper.socket")
        self.assertEqual(values(s, "Socket", "SocketMode"), ["0660"])
        self.assertEqual(values(s, "Socket", "SocketUser"), ["root"])
        self.assertEqual(values(s, "Socket", "SocketGroup"), ["@CLIENT_GROUP@"])
        self.assertEqual(values(s, "Socket", "Accept"), ["yes"])
        self.assertEqual(values(s, "Socket", "ListenStream"), ["/run/fc-vm-helper.sock"])
        h = unit("fc-vm-helper@.service")
        self.assertEqual(values(h, "Service", "StandardInput"), ["socket"])
        self.assertEqual(values(h, "Service", "ExecStart"), ["/usr/local/libexec/fc-vm-helper"])

    def test_nothing_new_touches_the_legacy_service_or_uses_sudo(self):
        for p in HERE.iterdir():
            if p.suffix in {".service", ".socket", ".sh", ".example", ".json", ".md"} or p.name.endswith(".service"):
                if p.name == "README.md":
                    continue
                text = "\n".join(l for l in p.read_text().splitlines() if not l.strip().startswith("#"))
                self.assertNotIn("firecracker-builder@", text, p.name)
                self.assertNotRegex(text, r"\bsudo\b", p.name)
                self.assertNotIn("ask-exec", text, p.name)

    def test_example_config_is_consistent_with_units_and_pool(self):
        cfg = json.loads((HERE / "vmd.example.json").read_text())["backend"]["local"]
        pool = dict(l.split("=") for l in (HERE / "pool.conf.example").read_text().splitlines() if l and not l.startswith("#"))
        self.assertEqual(cfg["guest_prefix"], pool["GUEST_PREFIX"])
        self.assertLessEqual(cfg["max_vms"], int(pool["MAX_SLOTS"]))
        self.assertEqual(cfg["pool_conf"], "/etc/firecracker-vms/pool.conf")
        self.assertEqual(cfg["storage_mount"], self.STORAGE)
        for key in ("work_root", "images_dir"):
            self.assertTrue(cfg[key].startswith(self.STORAGE + "/"), key)
        u = unit("firecracker-vm@.service")
        self.assertIn(f"{cfg['work_root']}:/work", values(u, "Service", "BindPaths"))
        self.assertEqual(f"{cfg['unit_mem_limit_mib']}M", values(u, "Service", "MemoryMax")[0])
        self.assertLessEqual(cfg["max_vms"] * cfg["profiles"][0]["mem_mib"], cfg["max_total_mem_mib"])
        for p in cfg["profiles"]:
            self.assertLessEqual(p["mem_mib"] + cfg["vmm_overhead_mib"], cfg["unit_mem_limit_mib"])
            self.assertLessEqual(p["mem_mib"], cfg["max_total_mem_mib"])
        # slice budget: aggregate guests must fit the legacy 2 GiB shared slice
        self.assertLessEqual(cfg["max_total_mem_mib"], 2048)
        self.assertLessEqual(cfg["max_total_vcpu"], 1)

    def test_scripts_have_valid_syntax(self):
        for name in ("fc-vm-network.sh",):
            subprocess.run(["bash", "-n", str(HERE / name)], check=True)


if __name__ == "__main__":
    unittest.main()
