#!/usr/bin/env python3
"""Owned IPv4 Docker publication allowances. Never modifies INPUT or policies."""
import fcntl
import hashlib
import ipaddress
import json
import os
import pathlib
import re
import shlex
import subprocess
import sys

CHAIN = 'LIDZA-PUBLIC'
MARKER = 'lidza-public-owner-v1'
JUMP = ['-m', 'comment', '--comment', MARKER, '-j', CHAIN]
RETURN = ['-m', 'comment', '--comment', MARKER, '-j', 'RETURN']
PREFIX = 'lidza-pub-v1-'

class Unsupported(Exception):
    pass

def run(*args, check=True):
    result = subprocess.run(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15)
    if check and result.returncode:
        raise Unsupported('Host public-port firewall command failed: ' + args[0] + '. Inspect host firewall/Docker configuration; no cloud firewall was changed.')
    return result

def ipt(*args, check=True):
    try:
        return run('iptables', '-w', '5', *args, check=check)
    except FileNotFoundError:
        raise Unsupported('Host public-port firewall requires iptables and rootful Docker with DOCKER-USER; native nftables/rootless backends are unsupported.')

def backend():
    if ipt('-S', 'DOCKER-USER', check=False).returncode:
        raise Unsupported('Docker DOCKER-USER chain is unavailable; rootless/native nftables/disabled Docker firewall backends are unsupported. Configure a rootful Docker iptables backend.')
    rules = [shlex.split(x) for x in ipt('-S', 'FORWARD').stdout.splitlines() if x.startswith('-A ')]
    if not rules or rules[0] != ['-A', 'FORWARD', '-j', 'DOCKER-USER']:
        raise Unsupported('Unsupported FORWARD ordering: DOCKER-USER must be the first forwarding rule. Preserve operator policies and review the host firewall; Deploy will not reorder unrelated rules.')

def comment(rule):
    try:
        return rule[rule.index('--comment') + 1]
    except (ValueError, IndexError):
        return ''

def chain_rules():
    result = ipt('-S', CHAIN, check=False)
    if result.returncode:
        return None
    return [shlex.split(line)[2:] for line in result.stdout.splitlines() if line.startswith('-A ')]

def owned_rules():
    rules = chain_rules()
    if rules is None:
        return None
    markers = [r for r in rules if r == RETURN]
    if len(markers) != 1 or rules[-1] != RETURN:
        raise Unsupported('Public-port chain name is occupied or its ownership marker was changed; no existing rules were replaced.')
    for rule in rules[:-1]:
        c = comment(rule)
        if not re.fullmatch(PREFIX + '[0-9a-f]{32}', c) or '-j' not in rule or rule[rule.index('-j')+1] != 'ACCEPT' or '--ctstate' not in rule or rule[rule.index('--ctstate')+1] != 'DNAT':
            raise Unsupported('Unrecognized rule in owned public-port chain; operator rules were preserved.')
    return rules[:-1]

def ensure_chain():
    rules = owned_rules()
    if rules is None:
        ipt('-N', CHAIN)
        ipt('-A', CHAIN, *RETURN)
        rules = []
    existing = [shlex.split(x)[2:] for x in ipt('-S', 'DOCKER-USER').stdout.splitlines() if x.startswith('-A ')]
    for rule in existing:
        if CHAIN in rule and rule != JUMP:
            raise Unsupported('An unowned jump references the public-port chain; it was preserved.')
    if existing.count(JUMP) > 1:
        raise Unsupported('Duplicate owned public-port jumps require operator review.')
    if not existing or existing[0] != JUMP:
        if JUMP in existing:
            ipt('-D', 'DOCKER-USER', *JUMP)
        ipt('-I', 'DOCKER-USER', '1', *JUMP)
    return rules

def rule_for(entry, address, port, protocol):
    # ctstate DNAT prevents this allow from matching directly routed private IPs.
    rule = ['-d', str(ipaddress.IPv4Address(address)) + '/32', '-p', protocol, '-m', protocol, '--dport', str(port), '-m', 'conntrack', '--ctstate', 'DNAT', '--ctdir', 'ORIGINAL', '--ctorigdstport', str(port)]
    identity = json.dumps([entry['owner'], entry['container'], rule], separators=(',', ':'))
    return rule + ['-m', 'comment', '--comment', PREFIX + hashlib.sha256(identity.encode()).hexdigest()[:32], '-j', 'ACCEPT']

def inspect(entries):
    names = run('docker', 'ps', '-a', '--format', '{{.Names}}').stdout.splitlines()
    selected = sorted(set(entries).intersection(names))
    if not selected:
        return {}
    template = '{"name":{{json .Name}},"running":{{json .State.Running}},"owner":{{json (index .Config.Labels "io.lidza.stack")}},"app":{{json (index .Config.Labels "io.lidza.app")}},"ports":{{json .NetworkSettings.Ports}},"networks":{{json .NetworkSettings.Networks}}}'
    result = run('docker', 'inspect', '--format', template, *selected)
    return {r['name'].lstrip('/'): r for r in map(json.loads, result.stdout.splitlines())}

def validate(entries):
    for name, entry in entries.items():
        if name != entry['container'] or not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9_.-]{0,200}', name):
            raise ValueError('Invalid public-port container registration')
        for p in entry.get('ports') or []:
            if type(p['port']) is not int or not 1 <= p['port'] <= 65535 or p['protocol'] not in ('tcp', 'udp') or not entry.get('owner'):
                raise ValueError('Unsupported declared public port or missing resource owner')

def desired(entries, containers):
    rules, rows, claims = {}, [], {}
    for name, entry in sorted(entries.items()):
        live = containers.get(name)
        if live and entry['ports'] and (live['owner'] != entry['owner'] or live['app'] != entry['app_id']):
            raise Unsupported('Public-port container ownership mismatch; no firewall allowance was created.')
        actual = (live or {}).get('ports') or {}
        ips = [n.get('IPAddress', '') for n in ((live or {}).get('networks') or {}).values() if n.get('IPAddress')]
        declared = {str(p['port'])+'/'+p['protocol'] for p in (entry.get('ports') or [])}
        for p in entry.get('ports') or []:
            key = str(p['port'])+'/'+p['protocol']
            bindings = actual.get(key) or []
            exact = [{'HostIp': '0.0.0.0', 'HostPort': str(p['port'])}]
            published = bool(live and live['running'] and bindings == exact and ips)
            row = dict(app_id=entry['app_id'], service=entry['service'], container=name, release=entry['release'], declared=True, protocol=p['protocol'], host_ip='0.0.0.0', host_port=p['port'], container_port=p['port'], publication='published' if published else ('missing' if not live else 'stopped' if not live['running'] else 'mismatch'), bindings=bindings, firewall='not_installed', external='unverified')
            if published:
                claim = (p['port'], p['protocol'])
                if claim in claims and claims[claim] != name:
                    raise Unsupported('Conflicting public host-port owners; no existing container was replaced.')
                claims[claim] = name
                for ip in ips:
                    rule = rule_for(entry, ip, p['port'], p['protocol'])
                    rules[comment(rule)] = rule
                row['_rules'] = [comment(rule_for(entry, ip, p['port'], p['protocol'])) for ip in ips]
            rows.append(row)
        for key, bindings in sorted(actual.items()):
            if key in declared:
                continue
            port, protocol = key.split('/')
            for binding in bindings or []:
                private = ipaddress.ip_address(binding['HostIp']).is_loopback
                rows.append(dict(app_id=entry['app_id'], service=entry['service'], container=name, release=entry['release'], declared=False, protocol=protocol, host_ip=binding['HostIp'], host_port=int(binding['HostPort']), container_port=int(port), publication='private_proxy' if private else 'undeclared', bindings=[binding], firewall='not_required' if private else 'not_managed', external='not_applicable' if private else 'unverified'))
    return rules, rows

def reconcile(rules):
    if not rules and chain_rules() is None:
        return
    # Stale allows can be removed even if Docker's forwarding hook changed.
    existing = owned_rules() or []
    for rule in existing:
        if comment(rule) not in rules:
            ipt('-D', CHAIN, *rule)
    if not rules:
        return
    backend()
    existing = ensure_chain()
    # Add new rules before deleting stale ones so shared owners keep access.
    for rule in rules.values():
        if ipt('-C', CHAIN, *rule, check=False).returncode:
            ipt('-I', CHAIN, '1', *rule)
    for rule in existing:
        if comment(rule) not in rules:
            ipt('-D', CHAIN, *rule)
    for rule in rules.values():
        if ipt('-C', CHAIN, *rule, check=False).returncode:
            raise Unsupported('Public-port firewall rule could not be verified')

def write_registry(root, entries):
    temporary = root/'registry.tmp'
    with temporary.open('w') as f:
        os.chmod(temporary, 0o600)
        json.dump(entries, f)
        f.flush()
        os.fsync(f.fileno())
    os.replace(temporary, root/'registry.json')
    directory = os.open(root, os.O_DIRECTORY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)

def main():
    request = json.load(sys.stdin)
    root = pathlib.Path(sys.argv[1])
    action = request.get('action', 'inspect')
    if action == 'check':
        backend()
        owned_rules()
        print(json.dumps({'ports': []}))
        return
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (root/'lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        path = root/'registry.json'
        entries = json.loads(path.read_text()) if path.exists() else {}
        entries.update({r['container']: r for r in request.get('registrations', [])})
        validate(entries)
        if action == 'register':
            # Write before launch. A crash is repaired by reconciliation on boot.
            write_registry(root, entries)
            print(json.dumps({'ports': []}))
            return
        live = inspect(entries)
        rules, rows = desired(entries, live)
        warning = ''
        try:
            if action == 'sync':
                reconcile(rules)
                write_registry(root, {name: entry for name, entry in entries.items() if name in live})
            if rules:
                backend()
                owned_rules()
                path_installed = ipt('-C', 'DOCKER-USER', *JUMP, check=False).returncode == 0
                for row in rows:
                    if row.get('_rules'):
                        row['firewall'] = 'allow_rule_installed' if path_installed and all(ipt('-C', CHAIN, *rules[key], check=False).returncode == 0 for key in row['_rules']) else 'not_installed'
        except Unsupported as error:
            if action == 'sync':
                raise
            warning = str(error)
            for row in rows:
                if row['declared']:
                    row['firewall'] = 'unsupported'
        for row in rows:
            row.pop('_rules', None)
        print(json.dumps({'ports': rows, 'warning': warning}))

if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # Arguments and container environment are never included in errors.
        print(json.dumps({'error': str(error) if isinstance(error, (Unsupported, ValueError)) else 'Public-port inspection/reconciliation failed; check Docker and host firewall availability.'}))
        sys.exit(1)
