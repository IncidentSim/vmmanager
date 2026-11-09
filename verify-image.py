#!/usr/bin/env python3
"""Boot a disposable image copy twice; verify SQL and persistence; use the systemd-managed VMM."""
import argparse, hashlib, http.client, json, os, pathlib, re, shutil, signal, socket, subprocess, tempfile, time
ROOT=pathlib.Path(__file__).resolve().parent
IMAGE=pathlib.Path(os.environ.get('FC_VERIFY_IMAGE',str(ROOT/'images/ubuntu-postgres.ext4')))
WORKSPACE=pathlib.Path('/var/lib/firecracker-builder/work')
SOCKET=os.environ.get('FC_SOCKET','/run/firecracker-builder/main.sock')
REPORT=pathlib.Path(os.environ.get('FC_VERIFY_REPORT',str(ROOT/'verification.json')))
def install_signal_handlers():
    def stop(signum, frame):
        # Unwind main's finally, and do not interrupt its cleanup a second time.
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, signal.SIG_IGN)
        raise SystemExit(128+signum)
    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, stop)
def digest(path):
    h=hashlib.sha256()
    with open(path,'rb') as f:
        for b in iter(lambda:f.read(1024*1024),b''): h.update(b)
    return h.hexdigest()
def check_ready():
    """Read-only preflight before a potentially multi-GB disposable copy.

    Configuration and all guest actions remain in the official Go SDK runner.
    """
    try:
        with socket.socket(socket.AF_UNIX,socket.SOCK_STREAM) as conn:
            conn.settimeout(2)
            conn.connect(SOCKET)
            conn.sendall(b'GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n')
            response=http.client.HTTPResponse(conn)
            response.begin()
            if response.status!=200:
                raise ValueError(f'HTTP {response.status}')
            info=json.loads(response.read(16385))
        if not isinstance(info,dict):
            raise ValueError('invalid InstanceInfo response')
        if info.get('state')!='Not started':
            raise ValueError('VMM is busy (expected Not started)')
        invocation=info.get('id','')
        if not isinstance(invocation,str) or not re.fullmatch('[0-9a-f]{32}',invocation):
            raise ValueError('invalid VMM invocation id')
        return invocation
    except (OSError,ValueError,http.client.HTTPException) as exc:
        raise RuntimeError(f'VMM socket {SOCKET}: {exc}; operator must start/restart the VMM externally') from exc

def wait_for_replacement(previous):
    # The first runner already proved the OLD exit by receipt. This wait is
    # only for externally configured autorestart, never evidence of clean exit.
    deadline=time.monotonic()+10
    while True:
        try:
            if check_ready()!=previous:
                return
        except RuntimeError:
            pass
        if time.monotonic()>=deadline:
            raise RuntimeError('Next VMM is not ready; operator must restart it externally')
        time.sleep(0.1)

def main():
    if not os.access(ROOT/'image-builder',os.X_OK):
        raise RuntimeError('Build the SDK runner first: go build -o image-builder .')
    check_ready()
    before=digest(IMAGE)
    work=pathlib.Path(tempfile.mkdtemp(prefix='fc-pg-verify-',dir=WORKSPACE))
    work.chmod(0o770)
    safe_cleanup=True
    vm=None
    try:
        disk=work/'running.ext4'
        subprocess.run(['cp','--reflink=auto','--sparse=always',str(IMAGE),str(disk)],check=True)
        disk.chmod(0o660)

        ssh=['ssh','-T','-i',str(ROOT/'assets/id_ed25519'),'-o','BatchMode=yes','-o','ConnectTimeout=2','-o','StrictHostKeyChecking=accept-new','-o','UserKnownHostsFile='+str(work/'known_hosts'),'-o','LogLevel=ERROR','root@172.16.0.2']
        invocations=[]
        for boot in (1,2):
            invocations.append(check_ready())
            with open(work/f'boot-{boot}.log','w') as log:
                safe_cleanup=False
                vm=subprocess.Popen([str(ROOT/'image-builder'),'-socket',SOCKET,'-run-image',str(disk)],stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
                deadline=time.monotonic()+90
                while True:
                    if vm.poll() is not None: raise RuntimeError((work/f'boot-{boot}.log').read_text()[-6000:])
                    probe=subprocess.run(ssh+['true'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
                    if probe.returncode==0: break
                    if time.monotonic()>deadline: raise TimeoutError('SSH timeout')
                    time.sleep(1)
                print(f'BOOT {boot}: SSH ready',flush=True)
                if boot==1:
                    script="""set -eu
cat /etc/os-release
uname -r
for attempt in $(seq 1 60); do
  if runuser -u postgres -- pg_isready -q; then break; fi
  sleep 1
done
runuser -u postgres -- pg_isready -q
runuser -u postgres -- psql -v ON_ERROR_STOP=1 -Atqc 'SHOW server_version; CREATE TABLE fc_smoke (id integer PRIMARY KEY, value text); INSERT INTO fc_smoke VALUES (1, $$persisted-across-reboot$$); SELECT value FROM fc_smoke WHERE id=1;'
"""
                else:
                    script="""set -eu
for attempt in $(seq 1 60); do
  if runuser -u postgres -- pg_isready -q; then break; fi
  sleep 1
done
runuser -u postgres -- pg_isready -q
runuser -u postgres -- psql -v ON_ERROR_STOP=1 -Atqc 'SELECT value FROM fc_smoke WHERE id=1;'
"""
                r=subprocess.run(ssh+['bash -se'],input=script,text=True,capture_output=True,timeout=90)
                print(r.stdout,flush=True)
                if r.returncode: raise RuntimeError(r.stderr)
                if 'persisted-across-reboot' not in r.stdout: raise RuntimeError('Persistence assertion failed')
                version=subprocess.run(ssh+["runuser -u postgres -- psql -Atqc 'SHOW server_version;'"],check=True,capture_output=True,text=True,timeout=15).stdout.strip()
                subprocess.run(ssh+["nohup sh -c 'sleep 2; sync; reboot' >/dev/null 2>&1 </dev/null &"],check=True,timeout=15)
                code=vm.wait(timeout=60);vm=None
                if code: raise RuntimeError(f'Runner exit {code}; disk preserved in {work}')
                safe_cleanup=True
            if boot==1:
                wait_for_replacement(invocations[-1])
        after=digest(IMAGE)
        assert before==after, 'Original output image unexpectedly modified'
        report={'verified':True,'vm_backend':'firecracker-go-sdk/socket','clean_exit_receipts':2,'boots':2,'postgresql':version,'persistence':'passed','output_image_sha256':after,'output_image_unchanged':True}
        REPORT.write_text(json.dumps(report,indent=2)+'\n')
        print(json.dumps(report,indent=2),flush=True)
    finally:
        # SIGTERM lets the runner request SendCtrlAltDel and await the old
        # process exit receipt. Never SIGKILL it or control the host service.
        if vm is not None:
            if vm.poll() is None:
                vm.terminate()
            try:
                safe_cleanup = vm.wait(timeout=40) == 0
            except subprocess.TimeoutExpired:
                safe_cleanup = False
        if safe_cleanup:
            shutil.rmtree(work)
        else:
            print(f'Working disk retained for safety: {work}',flush=True)
if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('-socket',default=SOCKET,help='existing Firecracker API Unix socket')
    parser.add_argument('--check-ready',action='store_true',help='read-only fail-fast readiness check')
    args=parser.parse_args()
    SOCKET=args.socket
    install_signal_handlers()
    if args.check_ready:
        print(check_ready())
    else:
        main()
