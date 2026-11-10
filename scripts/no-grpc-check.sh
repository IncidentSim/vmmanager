#!/usr/bin/env bash
# No application VM RPC transport. Transitive module metadata may remain through
# dependencies, but our source and deployed API/CLI build graphs must be RPC-free.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
# Keep go list outside a grep/||true pipeline: a broken package graph must fail.
go list -json ./... > "$work/packages.json"
go list -deps ./cmd/vmapi ./cmd/vmctl > "$work/binary-deps.txt"
go list -m -json all > "$work/modules.json"
go mod edit -json > "$work/go-mod.json"
python3 - "$work" <<'PY'
import json, pathlib, sys
work = pathlib.Path(sys.argv[1])
prefixes = ('google.golang.org/grpc', 'google.golang.org/protobuf',
            'google.golang.org/genproto', 'github.com/golang/protobuf')
def rpc(s):
    return any(s == p or s.startswith(p + '/') for p in prefixes)
def stream(path):
    s = path.read_text()
    decoder = json.JSONDecoder()
    while s.strip():
        s = s.lstrip()
        obj, end = decoder.raw_decode(s)
        yield obj
        s = s[end:]
errors = []
for package in stream(work / 'packages.json'):
    for field in ('Imports', 'TestImports', 'XTestImports'):
        for name in package.get(field, []):
            if rpc(name):
                errors.append(f'{package["ImportPath"]}: {field} imports {name}')
for gone in ('proto', 'vmlib/gen', 'vmlib/remote', 'cmd/vmd', 'scripts/gen-proto.sh', 'tools'):
    if pathlib.Path(gone).exists():
        errors.append(f'removed application RPC tree exists: {gone}')
for dep in json.loads((work / 'go-mod.json').read_text()).get('Require', []):
    if rpc(dep['Path']) and not dep.get('Indirect', False):
        errors.append(f'direct RPC requirement: {dep["Path"]}')
for name in (work / 'binary-deps.txt').read_text().splitlines():
    if rpc(name):
        errors.append(f'API/CLI binary dependency graph includes {name}')
transitive = [m['Path'] for m in stream(work / 'modules.json') if rpc(m['Path'])]
print('note: transitive RPC/protobuf module metadata: ' + (', '.join(transitive) or 'none'))
if errors:
    for error in errors:
        print('FAIL: ' + error, file=sys.stderr)
    sys.exit(1)
print('no-grpc-check ok: application source/tests and API/CLI dependency graphs are RPC-free')
PY
