"""Offline contract checks for the no-application-RPC gate, not a Go build."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parent / "no-grpc-check.sh"
FAKE_GO = r'''#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
mode = os.environ.get('GATE_CASE', '')
if args[:2] == ['list', '-json']:
    if mode == 'broken':
        sys.exit(7)
    print(json.dumps({'ImportPath': 'example/app', 'TestImports':
                     ['google.golang.org/grpc/status'] if mode == 'test-import' else []}))
elif args[:2] == ['list', '-deps']:
    print('google.golang.org/protobuf/proto' if mode == 'linked' else 'net/http')
elif args[:3] == ['list', '-m', '-json']:
    print(json.dumps({'Path': 'google.golang.org/grpc'}))
elif args[:2] == ['mod', 'edit']:
    print(json.dumps({'Require': [{'Path': 'google.golang.org/grpc', 'Indirect': mode != 'direct'}]}))
else:
    sys.exit(9)
'''


class NoGrpcGateTests(unittest.TestCase):
    def gate(self, case):
        with tempfile.TemporaryDirectory() as d:
            go = Path(d, 'go')
            go.write_text(FAKE_GO)
            go.chmod(0o700)
            env = dict(os.environ, PATH=d + os.pathsep + os.environ['PATH'], GATE_CASE=case)
            return subprocess.run(['bash', str(SCRIPT)], env=env, capture_output=True, text=True, timeout=30)

    def test_transitive_module_metadata_is_not_application_use(self):
        p = self.gate('')
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        self.assertIn('RPC-free', p.stdout)

    def test_import_binary_dependency_and_direct_module_are_rejected(self):
        for case in ('test-import', 'linked', 'direct'):
            with self.subTest(case=case):
                p = self.gate(case)
                self.assertNotEqual(p.returncode, 0, p.stdout + p.stderr)
                self.assertIn('FAIL:', p.stderr)

    def test_broken_go_list_is_not_hidden_as_empty_matches(self):
        p = self.gate('broken')
        self.assertEqual(p.returncode, 7, p.stdout + p.stderr)
        self.assertNotIn('RPC-free', p.stdout)


if __name__ == '__main__':
    unittest.main()
