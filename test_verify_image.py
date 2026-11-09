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
    v.check_ready=lambda:test_invocation
    test_invocation='0123456789abcdef0123456789abcdef'
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
    signal.pthread_sigmask(signal.SIG_UNBLOCK, {signal.SIGTERM, signal.SIGINT})
    v.main()
'''

import importlib.util
import tempfile
from unittest import mock

class VerifierReadinessTests(unittest.TestCase):
    def module(self):
        spec = importlib.util.spec_from_file_location('verify', Path(__file__).with_name('verify-image.py'))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_unix_preflight_validates_state_and_invocation(self):
        import json
        import socketserver
        import threading
        for payload, accepted in [
            ({'id': '0123456789abcdef0123456789abcdef', 'state': 'Not started'}, True),
            ({'id': '0123456789abcdef0123456789abcdef', 'state': 'Running'}, False),
            ({'id': '../escape', 'state': 'Not started'}, False),
            ({'id': 42, 'state': 'Not started'}, False),
            (None, False),
        ]:
            with self.subTest(payload=payload), tempfile.TemporaryDirectory() as d:
                body = json.dumps(payload).encode()
                class Handler(socketserver.StreamRequestHandler):
                    def handle(self):
                        while self.rfile.readline().strip():
                            pass
                        self.wfile.write(b'HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: '
                                         + str(len(body)).encode() + b'\r\n\r\n' + body)
                v = self.module()
                v.SOCKET = str(Path(d) / 'api.sock')
                with socketserver.UnixStreamServer(v.SOCKET, Handler) as server:
                    thread = threading.Thread(target=server.serve_forever, kwargs={'poll_interval': .01})
                    thread.start()
                    try:
                        if accepted:
                            self.assertEqual(v.check_ready(), payload['id'])
                        else:
                            with self.assertRaisesRegex(RuntimeError, 'externally'):
                                v.check_ready()
                    finally:
                        server.shutdown()
                        thread.join()

    def test_offline_fails_before_hashing_or_creating_working_copy(self):
        v = self.module()
        with tempfile.TemporaryDirectory() as d:
            v.SOCKET = str(Path(d) / 'missing.sock')
            with mock.patch.object(v.os, 'access', return_value=True), \
                 mock.patch.object(v, 'digest') as digest, \
                 mock.patch.object(v.tempfile, 'mkdtemp', side_effect=AssertionError('work created before readiness')) as mkdir, \
                 mock.patch.object(v.subprocess, 'run', side_effect=AssertionError('copy attempted')):
                with self.assertRaisesRegex(RuntimeError, 'externally'):
                    v.main()
                digest.assert_not_called()
                mkdir.assert_not_called()

class VerifierSignalTests(unittest.TestCase):
    def test_sigterm_stops_runner_without_privileged_network_cleanup(self):
        # Tool/worker threads can carry a blocked mask into a child process.
        old_mask = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGTERM})
        try:
            proc = subprocess.Popen([sys.executable, '-c', HARNESS], stdout=subprocess.PIPE,
                                    stderr=subprocess.STDOUT, text=True, cwd=Path(__file__).parent)
        finally:
            signal.pthread_sigmask(signal.SIG_SETMASK, old_mask)
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
