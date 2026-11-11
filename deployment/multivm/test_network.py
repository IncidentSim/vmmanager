"""Source-only firewall tests. All privileged commands are executable mocks."""
import json
import ipaddress
import os
import shlex
import shutil
from pathlib import Path
import subprocess
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
BASE = 'GUEST_PREFIX=10.213\nMAX_SLOTS=4\n'
PUBLIC = BASE + 'PUBLIC_IPV4=8.8.4.4\nSSH_PORT_BASE=22000\n'

# A deliberately small command/state model, not a replacement for off-host proof.
MOCK = r'''#!/usr/bin/env python3
import json, os, sys, fcntl
from pathlib import Path
cmd=Path(sys.argv[0]).name
# A restore waits for its producer before locking shared command state; otherwise
# the concurrent producer's -C call would race (or deadlock) with stdin reading.
payload=sys.stdin.read() if cmd.endswith('-restore') else ''
p=Path(os.environ['MOCK_STATE'])
lock=open(str(p)+'.lock','w'); fcntl.flock(lock,fcntl.LOCK_EX)
s=json.loads(p.read_text())
a=sys.argv[1:]
s['calls'].append([cmd]+a)
rc=0; out=''
if cmd=='ip':
    if a==['-4','route','show','table','all']:
        out=s.get('routes','default via 8.8.4.1 dev eth0\n')
    elif a==['-4','-o','route','show','default']:
        out=s.get('defaults','default via 8.8.4.1 dev eth0\n')
    elif a==['-4','-o','addr','show']:
        out='2: eth0 inet 8.8.4.4/24 scope global eth0\n'
        if s['tap']: out+='3: tap-vm0 inet 10.213.0.1/30 scope global tap-vm0\n'
    elif a==['-4','-o','addr','show','dev','tap-vm0']:
        out='3: tap-vm0 inet 10.213.0.1/30 scope global tap-vm0\n' if s['tap'] else ''
    elif a[:2]==['tuntap','show']: out=s.get('tuntap_output','tap-vm0: tap persist\n')
    elif a[:2]==['link','show']: rc=0 if s['tap'] else 1
    elif a[:2]==['tuntap','add']: s['tap']=True
    elif a[:2]==['link','del']: s['tap']=False
elif cmd=='sysctl': out='1\n'
elif cmd=='conntrack': rc=s.get('conntrack_rc',0)
elif cmd.endswith('-restore'):
    s['payloads'].append([cmd,payload])
    table='filter'
    for line in payload.splitlines():
        if line.startswith('*'): table=line[1:]
        elif line.startswith(':'):
            name=line.split()[0][1:]; s['rules'].setdefault(cmd+':'+table+':'+name,[])
        elif line.startswith('-F '): s['rules'][cmd+':'+table+':'+line.split()[1]]=[]
        elif line.startswith(('-A ','-I ','-D ')):
            import shlex
            words=shlex.split(line); key=cmd+':'+table+':'+words[1]
            rows=s['rules'].setdefault(key,[])
            if words[0]=='-I': rows.insert(0,words[2:])
            elif words[0]=='-D': rows.remove(words[2:])
            else: rows.append(words[2:])
elif cmd in ('iptables-nft','ip6tables-nft'):
    restore=cmd+'-restore'; table='filter'
    if a[:1]==['-w']: a=a[1:]
    if a[:1]==['-t']: table=a[1]; a=a[2:]
    op,chain,*rule=a; key=restore+':'+table+':'+chain
    rows=s['rules'].setdefault(key,[])
    if op=='-C': rc=0 if rule in rows else 1
    elif op=='-I': rows.insert(0,rule)
    elif op=='-D':
        if rule in rows: rows.remove(rule)
        else: rc=1
    elif op=='-F': s['rules'][key]=[]
    elif op=='-X': s['rules'].pop(key,None)
p.write_text(json.dumps(s)); print(out,end=''); sys.exit(rc)
'''


class FirewallTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.d = Path(self.tmp.name)
        (self.d/'bin').mkdir()
        for name in ('ip','sysctl','conntrack','iptables-nft','ip6tables-nft','iptables-nft-restore','ip6tables-nft-restore'):
            f=self.d/'bin'/name; f.write_text(MOCK); f.chmod(0o700)
        self.pool=self.d/'pool'; self.pool.write_text(PUBLIC)
        self.state=self.d/'mock.json'
        self.state.write_text(json.dumps({'tap':True,'calls':[],'payloads':[], 'rules':{'iptables-nft-restore:filter:FORWARD':[['-j','FOREIGN']]}}))
        code=(HERE/'fc-vm-network.sh').read_text().replace('/etc/firecracker-vms/pool.conf',str(self.pool)).replace('/run/firecracker-vms-net',str(self.d/'run')).replace('PATH=/usr/sbin:/usr/bin:/sbin:/bin',f'PATH={self.d}/bin:/usr/bin:/bin').replace('$EUID == 0','1 == 1')
        self.script=self.d/'net.sh'; self.script.write_text(code)

    def run_net(self, action='refresh'):
        return subprocess.run(['bash',str(self.script),action,'0'],text=True,capture_output=True,env={**os.environ,'MOCK_STATE':str(self.state)})

    def data(self): return json.loads(self.state.read_text())

    def test_tuntap_show_may_return_all_devices(self):
        state=self.data()
        state['tuntap_output']='tap-vm9: tap vnet_hdr persist\ntap-vm0: tap vnet_hdr persist\ntap-vm2: tap persist\n'
        self.state.write_text(json.dumps(state))
        p=self.run_net()
        self.assertEqual(p.returncode,0,p.stderr)

    def test_tuntap_requested_device_must_itself_be_tap(self):
        state=self.data()
        state['tuntap_output']='tap-vm9: tap persist\ntap-vm0: tun persist\n'
        self.state.write_text(json.dumps(state))
        p=self.run_net()
        self.assertNotEqual(p.returncode,0)
        self.assertIn('not the expected TAP',p.stderr)

    def test_refresh_existing_tap_is_non_destructive_and_idempotent(self):
        p=self.run_net(); self.assertEqual(p.returncode,0,p.stderr)
        first=self.data()['rules']
        p=self.run_net(); self.assertEqual(p.returncode,0,p.stderr)
        self.assertEqual(first,self.data()['rules'])
        self.assertIn(['-j','FOREIGN'],first['iptables-nft-restore:filter:FORWARD'])
        calls=self.data()['calls']
        self.assertFalse(any(c[0]=='ip' and c[1:3] in (['link','del'],['link','set'],['addr','add'],['tuntap','add']) for c in calls))
        self.assertFalse(any(c[0]=='conntrack' for c in calls))

    def test_refresh_reasserts_hook_precedence_after_foreign_insert(self):
        p=self.run_net(); self.assertEqual(p.returncode,0,p.stderr)
        s=self.data(); s['rules']['iptables-nft-restore:filter:FORWARD'].insert(0,['-j','FOREIGN_ACCEPT'])
        self.state.write_text(json.dumps(s))
        p=self.run_net(); self.assertEqual(p.returncode,0,p.stderr)
        rows=self.data()['rules']['iptables-nft-restore:filter:FORWARD']
        self.assertEqual(rows[0][-1],'FCVM0F')
        self.assertEqual(rows[1][-1],'FCVM0F')
        self.assertIn(['-j','FOREIGN_ACCEPT'],rows)

    def test_real_rootless_nft_translation_of_every_emitted_rule(self):
        if not shutil.which('iptables-translate') or not shutil.which('ip6tables-translate'):
            self.skipTest('rootless nft translation tools not installed')
        p=self.run_net(); self.assertEqual(p.returncode,0,p.stderr)
        for command,payload in self.data()['payloads']:
            translator='ip6tables-translate' if command.startswith('ip6') else 'iptables-translate'
            table='filter'
            for line in payload.splitlines():
                if line.startswith('*'): table=line[1:]
                if not line.startswith(('-A ','-I ','-D ')): continue
                out=subprocess.run([translator,'-t',table]+shlex.split(line),capture_output=True,text=True)
                self.assertEqual(out.returncode,0,line+'\n'+out.stderr)
                self.assertIn('nft ',out.stdout,line)
                self.assertNotIn('nft #',out.stdout,line)

    def test_nat_and_filters_are_narrow(self):
        p=self.run_net(); self.assertEqual(p.returncode,0,p.stderr)
        text='\n'.join(p for _,p in self.data()['payloads'])
        self.assertIn('-i eth0 -d 8.8.4.4/32 -p tcp --dport 22000',text)
        self.assertIn('DNAT --to-destination 10.213.0.2:22',text)
        self.assertIn('! -s 10.213.0.2/32 -j DROP',text)
        self.assertIn('-d 10.0.0.0/8 -j DROP',text)
        self.assertIn('-d 169.254.0.0/16 -j DROP',text)
        self.assertIn('-o eth0 -j ACCEPT',text)
        self.assertIn('MASQUERADE',text)
        self.assertNotIn('SNAT',text)
        self.assertIn('--ctdir REPLY',text)
        v6=[p for c,p in self.data()['payloads'] if c=='ip6tables-nft-restore']
        self.assertTrue(v6); self.assertIn('-j DROP',v6[0])

    def test_public_options_are_paired_strict_and_not_evaluated(self):
        for extra in ('PUBLIC_IPV4=8.8.4.4\n','SSH_PORT_BASE=22000\n', 'PUBLIC_IPV4=008.8.4.4\nSSH_PORT_BASE=22000\n', 'PUBLIC_IPV4=10.0.0.1\nSSH_PORT_BASE=22000\n', 'PUBLIC_IPV4=8.8.4.4\nSSH_PORT_BASE=65534\n', 'PUBLIC_IPV4=8.8.4.4\nSSH_PORT_BASE=022000\n', 'PUBLIC_IPV4=$(id)\nSSH_PORT_BASE=22000\n'):
            with self.subTest(extra=extra):
                self.pool.write_text(BASE+extra)
                p=self.run_net('check'); self.assertNotEqual(p.returncode,0)
                self.assertEqual(self.data()['calls'],[])

    def test_disabled_default_has_no_dnat(self):
        self.pool.write_text(BASE)
        p=self.run_net(); self.assertEqual(p.returncode,0,p.stderr)
        text='\n'.join(p for _,p in self.data()['payloads'])
        self.assertNotIn('-j DNAT',text)

    def test_down_never_deletes_unowned_tap(self):
        p=self.run_net('down'); self.assertNotEqual(p.returncode,0)
        self.assertIn('unowned existing TAP',p.stderr)
        self.assertTrue(self.data()['tap'])

    def verdict(self, chain, **packet):
        """Evaluate the emitted match subset, failing on any unmodelled option."""
        rows=self.data()['rules']['iptables-nft-restore:filter:'+chain]
        names={'-i':'i','-o':'o','-p':'proto','--sport':'sport','--dport':'dport',
               '--ctstate':'state','--ctdir':'dir','--ctstatus':'status',
               '--ctorigdst':'origdst','--ctorigdstport':'origport'}
        for row in rows:
            match=True; negate=False; it=iter(row)
            for key in it:
                if key=='!': negate=True; continue
                value=next(it)
                if key=='-m': continue
                if key=='-j':
                    if match: return value
                    break
                if key in ('-s','-d'):
                    addr=packet.get('src' if key=='-s' else 'dst','0.0.0.0')
                    result=ipaddress.ip_address(addr) in ipaddress.ip_network(value)
                else:
                    self.assertIn(key,names)
                    if key=='--ctstate' and value=='DNAT':
                        # DNAT is a virtual ctstate, not a --ctstatus value.
                        result=packet.get('status')=='DNAT'
                    else:
                        result=str(packet.get(names[key],'')) in value.split(',')
                match=match and (not result if negate else result)
                negate=False
        return 'RETURN'

    def test_guest_spoof_host_private_and_internet_policy(self):
        p=self.run_net(); self.assertEqual(p.returncode,0,p.stderr)
        guest={'i':'tap-vm0','o':'eth0','src':'10.213.0.2','dst':'1.1.1.1','proto':'udp','dport':53,'state':'NEW','dir':'ORIGINAL'}
        self.assertEqual(self.verdict('FCVM0F',**guest),'ACCEPT')
        self.assertEqual(self.verdict('FCVM0F',**{**guest,'dst':'9.9.9.9','proto':'tcp','dport':443}),'ACCEPT')
        self.assertEqual(self.verdict('FCVM0F',**{**guest,'src':'10.213.1.2'}),'DROP')
        for dst in ('10.213.1.2','169.254.169.254','100.100.100.200','192.168.1.1','127.0.0.1'):
            self.assertEqual(self.verdict('FCVM0F',**{**guest,'dst':dst}),'DROP',dst)
        # New INPUT is denied even for host's public address; host-originated
        # SSH diagnostics can receive conntrack REPLY packets.
        self.assertEqual(self.verdict('FCVM0I',**{**guest,'dst':'8.8.4.4'}),'DROP')
        self.assertEqual(self.verdict('FCVM0I',**{**guest,'state':'ESTABLISHED','dir':'REPLY'}),'ACCEPT')
        inbound={'i':'eth0','o':'tap-vm0','src':'9.9.9.9','dst':'10.213.0.2','proto':'tcp','dport':22,'state':'NEW','dir':'ORIGINAL','status':'DNAT','origdst':'8.8.4.4','origport':22000}
        self.assertEqual(self.verdict('FCVM0F',**inbound),'ACCEPT')
        for diff in ({'dport':80},{'origport':22001},{'origdst':'8.8.8.8'},{'status':''},{'i':'wg0'}):
            self.assertEqual(self.verdict('FCVM0F',**{**inbound,**diff}),'DROP')
        # DNAT replies preserve the external client's address, including clients
        # whose source arrives over the public uplink from a private network.
        reply={**guest,'proto':'tcp','sport':22,'state':'ESTABLISHED','dir':'REPLY','status':'DNAT','origdst':'8.8.4.4','origport':22000,'dst':'192.168.1.2'}
        self.assertEqual(self.verdict('FCVM0F',**reply),'ACCEPT')
        self.assertEqual(self.verdict('FCVM0F',**{**reply,'dir':'ORIGINAL'}),'DROP')

    def test_vpn_global_route_denied_and_conntrack_failure_blocks_creation(self):
        s=self.data(); s['routes']='default via 8.8.4.1 dev eth0\n9.9.0.0/16 dev wg0 table 100\n'
        self.state.write_text(json.dumps(s))
        p=self.run_net(); self.assertEqual(p.returncode,0,p.stderr)
        self.assertEqual(self.verdict('FCVM0F',i='tap-vm0',o='eth0',src='10.213.0.2',dst='9.9.9.9',state='NEW'),'DROP')
        s=self.data(); s['tap']=False; s['calls']=[]; s['conntrack_rc']=1
        self.state.write_text(json.dumps(s))
        p=self.run_net('up'); self.assertNotEqual(p.returncode,0)
        self.assertIn('conntrack deletion failed',p.stderr)
        self.assertFalse(self.data()['tap'])

    def test_new_tap_is_protected_before_link_up_and_cleans_conntrack(self):
        s=self.data(); s['tap']=False; self.state.write_text(json.dumps(s))
        p=self.run_net('up'); self.assertEqual(p.returncode,0,p.stderr)
        calls=self.data()['calls']
        flush=next(i for i,c in enumerate(calls) if c[0]=='conntrack')
        create=next(i for i,c in enumerate(calls) if c[:3]==['ip','tuntap','add'])
        active=next(i for i,c in enumerate(calls) if c[:3]==['ip','link','set'])
        self.assertLess(flush,create)
        self.assertLess(max(i for i,c in enumerate(calls) if c[0].endswith('-restore')),active)
        p=self.run_net('up'); self.assertEqual(p.returncode,0,p.stderr)
        self.assertEqual(sum(c[0]=='conntrack' for c in self.data()['calls']),1)
        p=self.run_net('down'); self.assertEqual(p.returncode,0,p.stderr)
        self.assertFalse(self.data()['tap'])
        p=self.run_net('down'); self.assertEqual(p.returncode,0,p.stderr)
        self.assertIn(['-j','FOREIGN'],self.data()['rules']['iptables-nft-restore:filter:FORWARD'])

    def test_explicit_conntrack_flush_scopes_original_tuple(self):
        p=self.run_net('flush-conntrack'); self.assertEqual(p.returncode,0,p.stderr)
        calls=[c for c in self.data()['calls'] if c[0]=='conntrack']
        self.assertEqual(calls,[['conntrack','-D','-f','ipv4','-p','tcp','--orig-dst','8.8.4.4','--dport','22000']])

if __name__=='__main__': unittest.main()
