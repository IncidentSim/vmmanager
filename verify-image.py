#!/usr/bin/env python3
"""Boot a disposable image copy twice; verify SQL and persistence; remove demo networking."""
import hashlib, json, os, pathlib, shutil, signal, subprocess, tempfile, time
ROOT=pathlib.Path(__file__).resolve().parent
IMAGE=pathlib.Path(os.environ.get('FC_VERIFY_IMAGE',str(ROOT/'images/ubuntu-postgres.ext4')))
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
def main():
    assert os.geteuid()==0, 'Run through sudo ask-exec'
    if subprocess.run(['ip','link','show','tap-fcpg'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL).returncode==0:
        raise RuntimeError('tap-fcpg already exists; refusing to touch another VM/network')
    if not os.access(ROOT/'image-builder',os.X_OK):
        raise RuntimeError('Build the SDK runner first: go build -o image-builder .')
    before=digest(IMAGE)
    work=pathlib.Path(tempfile.mkdtemp(prefix='fc-pg-verify-',dir=os.environ['TMPDIR']))
    vm=None
    try:
        subprocess.run(['bash',str(ROOT/'network.sh'),'up'],check=True)
        disk=work/'running.ext4'
        subprocess.run(['cp','--reflink=auto','--sparse=always',str(IMAGE),str(disk)],check=True)

        ssh=['ssh','-T','-i',str(ROOT/'assets/id_ed25519'),'-o','BatchMode=yes','-o','ConnectTimeout=2','-o','StrictHostKeyChecking=accept-new','-o','UserKnownHostsFile='+str(work/'known_hosts'),'-o','LogLevel=ERROR','root@172.16.0.2']
        for boot in (1,2):
            with open(work/f'boot-{boot}.log','w') as log:
                vm=subprocess.Popen([str(ROOT/'image-builder'),'-run-image',str(disk),'-kernel',str(ROOT/'assets/vmlinux'),'-firecracker',str(ROOT/'assets/firecracker')],stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
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
  if systemctl is-active --quiet postgresql@16-main && runuser -u postgres -- pg_isready -q; then break; fi
  sleep 1
done
systemctl is-active postgresql@16-main
runuser -u postgres -- psql -v ON_ERROR_STOP=1 -Atqc 'SHOW server_version; CREATE TABLE fc_smoke (id integer PRIMARY KEY, value text); INSERT INTO fc_smoke VALUES (1, $$persisted-across-reboot$$); SELECT value FROM fc_smoke WHERE id=1;'
"""
                else:
                    script="""set -eu
for attempt in $(seq 1 60); do
  if systemctl is-active --quiet postgresql@16-main && runuser -u postgres -- pg_isready -q; then break; fi
  sleep 1
done
systemctl is-active postgresql@16-main
runuser -u postgres -- psql -v ON_ERROR_STOP=1 -Atqc 'SELECT value FROM fc_smoke WHERE id=1;'
"""
                r=subprocess.run(ssh+['bash -se'],input=script,text=True,capture_output=True,timeout=90)
                print(r.stdout,flush=True)
                if r.returncode: raise RuntimeError(r.stderr)
                if 'persisted-across-reboot' not in r.stdout: raise RuntimeError('Persistence assertion failed')
                version=subprocess.run(ssh+["runuser -u postgres -- psql -Atqc 'SHOW server_version;'"],check=True,capture_output=True,text=True,timeout=15).stdout.strip()
                subprocess.run(ssh+["nohup sh -c 'sleep 2; sync; reboot' >/dev/null 2>&1 </dev/null &"],check=True,timeout=15)
                code=vm.wait(timeout=60);vm=None
                if code: raise RuntimeError(f'Firecracker exit {code}')
        after=digest(IMAGE)
        assert before==after, 'Original output image unexpectedly modified'
        report={'verified':True,'vm_backend':'firecracker-go-sdk','boots':2,'postgresql':version,'persistence':'passed','output_image_sha256':after,'output_image_unchanged':True}
        REPORT.write_text(json.dumps(report,indent=2)+'\n')
        print(json.dumps(report,indent=2),flush=True)
    finally:
        if vm is not None and vm.poll() is None:
            vm.terminate()
            try: vm.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(vm.pid,signal.SIGKILL)
                vm.wait()
        shutil.rmtree(work)
        subprocess.run(['bash',str(ROOT/'network.sh'),'down'],check=True)
        uid,gid=1000,1000
        for d in (ROOT/'assets',ROOT/'images'):
            os.chown(d,uid,gid)
            for f in d.iterdir():
                if f.is_file(): os.chown(f,uid,gid)
        if REPORT.exists():os.chown(REPORT,uid,gid)
if __name__=='__main__':
    install_signal_handlers()
    main()
