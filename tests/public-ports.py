#!/usr/bin/env python3
import copy
import importlib.util
import pathlib
import subprocess
import sys
import unittest
from unittest.mock import patch
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location('ports', pathlib.Path(__file__).resolve().parents[1]/'internal/agent/public-ports.py')
p = importlib.util.module_from_spec(spec); spec.loader.exec_module(p)

def entry(name='one'):
    return dict(app_id=name, owner='incarnation-'+name, release='release', container=name, service='app', ports=[dict(port=25, protocol='tcp')])
def live(name='one', ip='172.18.0.2'):
    return dict(name=name, running=True, owner='incarnation-'+name, app=name, ports={'25/tcp':[dict(HostIp='0.0.0.0',HostPort='25')], '3000/tcp':[dict(HostIp='127.0.0.1',HostPort='32790')]}, networks={'private':dict(IPAddress=ip)})

class IPTables:
    def __init__(self):
        self.chains={'FORWARD':[['-j','DOCKER-USER'],['-j','OPERATOR']], 'DOCKER-USER':[['-j','RETURN']]}
        self.calls=[]
    def __call__(self,*args,check=True):
        self.calls.append(args); operation,chain,*rule=args
        if operation=='-S':
            code=0 if chain in self.chains else 1
            text='\n'.join('-A '+chain+' '+' '.join(r) for r in self.chains.get(chain,[]))
        elif operation=='-N': self.chains[chain]=[];code=0;text=''
        elif operation=='-C': code=0 if rule in self.chains.get(chain,[]) else 1;text=''
        elif operation=='-A': self.chains[chain].append(rule);code=0;text=''
        elif operation=='-I': self.chains[chain].insert(int(rule[0])-1,rule[1:]);code=0;text=''
        elif operation=='-D': self.chains[chain].remove(rule);code=0;text=''
        else: raise AssertionError(args)
        if check and code: raise p.Unsupported('fixture command failed')
        return subprocess.CompletedProcess(args,code,text,'')

class Tests(unittest.TestCase):
    def test_exact_dnat_rules_and_private_bindings(self):
        rules,rows=p.desired({'one':entry()},{'one':live()})
        rule=next(iter(rules.values()))
        self.assertIn('172.18.0.2/32',rule);self.assertIn('DNAT',rule);self.assertIn('--ctorigdstport',rule)
        self.assertEqual(rows[0]['external'],'unverified')
        self.assertEqual(rows[1]['firewall'],'not_required')
        self.assertEqual(rows[1]['host_ip'],'127.0.0.1')
        wrong=live();wrong['ports']['25/tcp'][0]['HostIp']='127.0.0.1'
        rules,rows=p.desired({'one':entry()},{'one':wrong})
        self.assertFalse(rules);self.assertEqual(rows[0]['publication'],'mismatch')
    def test_ownership_conflicts_fail_closed(self):
        wrong=live();wrong['owner']='someone-else'
        with self.assertRaises(p.Unsupported):p.desired({'one':entry()},{'one':wrong})
        with self.assertRaises(p.Unsupported):p.desired({'one':entry(),'two':entry('two')},{'one':live(),'two':live('two','172.18.0.3')})
    def test_shared_lifecycle_idempotence_preserves_operator_policy(self):
        table=IPTables();before=copy.deepcopy(table.chains)
        one,_=p.desired({'one':entry()},{'one':live()})
        other=entry('two');other['ports']=[dict(port=7882,protocol='udp')]
        two_live=live('two','172.18.0.3');two_live['ports']={'7882/udp':[dict(HostIp='0.0.0.0',HostPort='7882')]}
        two,_=p.desired({'two':other},{'two':two_live})
        with patch.object(p,'ipt',table):
            p.reconcile(one|two);snapshot=copy.deepcopy(table.chains)
            p.reconcile(one|two);self.assertEqual(snapshot,table.chains)
            p.reconcile(two);self.assertEqual(len(table.chains[p.CHAIN]),2)
            p.reconcile({});self.assertEqual(table.chains[p.CHAIN],[p.RETURN])
        self.assertEqual(table.chains['FORWARD'],before['FORWARD'])
        self.assertIn(['-j','RETURN'],table.chains['DOCKER-USER'])
        self.assertFalse(any('-F' in c or '-P' in c for c in table.calls))
    def test_foreign_chain_and_unsupported_forward_order_preserved(self):
        table=IPTables();table.chains[p.CHAIN]=[['-j','DROP']]
        with patch.object(p,'ipt',table):
            with self.assertRaises(p.Unsupported):p.ensure_chain()
        self.assertEqual(table.chains[p.CHAIN],[['-j','DROP']])
        table=IPTables();table.chains['FORWARD'].insert(0,['-j','OPERATOR'])
        before=copy.deepcopy(table.chains)
        with patch.object(p,'ipt',table):
            with self.assertRaises(p.Unsupported):p.backend()
        self.assertEqual(before,table.chains)
    def test_stopped_release_removes_allow_and_private_ports_stay_private(self):
        stopped=live();stopped['running']=False
        rules,rows=p.desired({'one':entry()},{'one':stopped})
        self.assertFalse(rules);self.assertEqual(rows[0]['publication'],'stopped')
        missing_rules,rows=p.desired({'one':entry()},{})
        self.assertFalse(missing_rules);self.assertEqual(rows[0]['publication'],'missing')
if __name__=='__main__':unittest.main()
