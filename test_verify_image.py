"""Unprivileged signal/cleanup regression; no KVM, network or root operations."""

from pathlib import Path
import selectors
import signal
import subprocess
import sys
import unittest

HARNESS = r'''
import importlib.util, os, pathlib, signal, tempfile, types
spec=importlib.util.spec_from_file_location('verify', 'verify-image.py')
v=importlib.util.module_from_spec(spec);spec.loader.exec_module(v)
with tempfile.TemporaryDirectory(dir=os.environ['TMPDIR']) as root:
    v.ROOT=pathlib.Path(root)
    for name in ('assets','images'): (v.ROOT/name).mkdir()
    v.REPORT=v.ROOT/'report.json'
    v.digest=lambda path:'unchanged'
    v.WORKSPACE=v.ROOT/'work';v.WORKSPACE.mkdir()
    v.os.access=lambda *args:True
    class VM:
        def poll(self): return None
        def terminate(self): print('VM_STOP',flush=True)
        def wait(self,timeout=None): return 0
    v.subprocess.Popen=lambda *args,**kwargs:VM()
    def run(args,**kwargs):
        if args[0]=='cp':
            pathlib.Path(args[-1]).write_bytes(b'disk')
        if args[0]=='ssh':
            print('READY',flush=True)
            signal.pause()
            raise RuntimeError('signal did not interrupt wait')
        return types.SimpleNamespace(returncode=0)
    v.subprocess.run=run
    getattr(v,'install_signal_handlers',lambda:None)()
    v.main()
'''

class VerifierSignalTests(unittest.TestCase):
    def test_sigterm_stops_runner_without_privileged_network_cleanup(self):
        proc = subprocess.Popen([sys.executable, '-c', HARNESS], stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, text=True, cwd=Path(__file__).parent)
        assert proc.stdout is not None
        try:
            with selectors.DefaultSelector() as sel:
                sel.register(proc.stdout, selectors.EVENT_READ)
                self.assertTrue(sel.select(timeout=10), 'harness did not become ready')
            self.assertEqual(proc.stdout.readline().strip(), 'READY')
            proc.send_signal(signal.SIGTERM)
            out, _ = proc.communicate(timeout=10)
            self.assertIn('VM_STOP', out)
            self.assertNotIn('NETWORK_DOWN', out)
            self.assertEqual(proc.returncode, 128 + signal.SIGTERM)
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
            proc.stdout.close()

if __name__ == '__main__':
    unittest.main()
