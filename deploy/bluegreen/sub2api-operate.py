#!/usr/bin/env python3
"""Sub2API blue/green release on dedicated-YosHub; never recreate data services."""
import argparse
import copy
import json
import os
import pathlib
import re
import subprocess
import sys
import tempfile
import time
import urllib.request

ROOT = pathlib.Path('/opt/yoshub-production')
COMPOSE = ROOT / 'sub2api.compose.json'
STATE = ROOT / 'sub2api-state.json'
ROUTER_DIR = ROOT / 'sub2api-router'
UPSTREAM = ROUTER_DIR / 'route.conf'
SITE = pathlib.Path('/etc/nginx/conf.d/sub2-yoshub.conf')
LEGACY_COMPOSE = ROOT / 'other-services.compose.json'
LEGACY = 'yoshub-sub2api'
ROUTER = 'sub2api-router'
SLOTS = {'blue': ('sub2api-blue', 8082), 'green': ('sub2api-green', 8083)}
NETWORKS = ['yoshub-sub2api_default', 'new-api_default']
ROUTER_PORT = 8084
INFRA = ['migration-postgres', 'migration-redis-sub2api', 'migration-mysql',
         'migration-redis-newapi', 'cli-proxy-api', 'new-api-cutover-blue',
         'new-api-cutover-green', 'new-api-cutover-controller']


def command(args, timeout=45):
    result = subprocess.run(args, capture_output=True, timeout=timeout)
    if result.returncode:
        atomic(ROOT / 'sub2api-command-error.log', result.stdout.decode(errors='replace') +
               result.stderr.decode(errors='replace'))
        raise RuntimeError(f'{args[0]} failed ({result.returncode}); see protected sub2api-command-error.log')
    return result.stdout.decode()


def atomic(path, text, mode=0o600):
    fd, name = tempfile.mkstemp(dir=path.parent, prefix='.' + path.name)
    try:
        with os.fdopen(fd, 'w') as handle:
            handle.write(text)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(name, mode)
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def backup(path):
    target = ROOT / 'backups' / ('sub2-' + str(time.time_ns()) + '-' + path.name)
    target.parent.mkdir(mode=0o700, exist_ok=True)
    atomic(target, path.read_text())
    if target.read_bytes() != path.read_bytes():
        raise RuntimeError('Configuration backup mismatch')


def save_state(state):
    atomic(STATE, json.dumps(state, indent=2) + '\n')


def inspect(name):
    return json.loads(command(['docker', 'inspect', name]))[0]


def infrastructure():
    values = {}
    for name in INFRA:
        item = inspect(name)
        if not item['State']['Running']:
            raise RuntimeError(name + ' must be running')
        values[name] = [item['Id'], item['State']['StartedAt']]
    return values


def validate_image(image):
    if not re.fullmatch(r'ghcr\.io/hdwwww/yoshub-sub2api:[\w.-]+@sha256:[0-9a-f]{64}', image or ''):
        raise RuntimeError('Use an immutable private Sub2API tag@sha256:digest')


def fetch(url):
    # Production hosts can have HTTP proxy environment variables; loopback is direct.
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(url, timeout=8) as reply:
        return json.load(reply), reply.headers


def health(slot):
    name, port = SLOTS[slot]
    item = inspect(name)
    if not item['State']['Running'] or not set(NETWORKS).issubset(item['NetworkSettings']['Networks']):
        raise RuntimeError(slot + ' is not running on both required networks')
    body, _ = fetch(f'http://127.0.0.1:{port}/health')
    if body.get('status') != 'ok':
        raise RuntimeError(slot + ' health failed')
    public, _ = fetch(f'http://127.0.0.1:{port}/api/v1/settings/public')
    return {'slot': slot, 'id': item['Id'], 'image': item['Config']['Image'],
            'version': public.get('data', {}).get('version')}


def slot_status(slot):
    found = command(['docker', 'ps', '-aq', '--filter', 'name=^/' + SLOTS[slot][0] + '$']).strip()
    if not found:
        return {'slot': slot, 'running': False, 'created': False}
    item = inspect(SLOTS[slot][0])
    if item['State']['Running']:
        return health(slot)
    return {'slot': slot, 'running': False, 'image': item['Config']['Image'], 'id': item['Id']}


def route_text(slot):
    if slot not in SLOTS:
        raise RuntimeError('Unknown customer slot')
    # A single active backend makes old-slot drain deterministic. A healthy standby
    # stays available for explicit rollback, without attracting new old-version work.
    return f'''upstream sub2api_active {{
    server {SLOTS[slot][0]}:8080;
    keepalive 64;
}}
map $host $sub2api_slot {{ default {slot}; }}
'''


def route():
    matches = re.findall(r'server sub2api-(blue|green):8080;', UPSTREAM.read_text())
    if len(matches) != 1:
        raise RuntimeError('Unrecognized Sub2API route')
    return matches[0]


ROUTER_CONFIG = '''worker_processes auto;
pid /tmp/nginx.pid;
error_log /dev/stderr warn;
events { worker_connections 16384; }
http {
    underscores_in_headers on;
    access_log off;
    resolver 127.0.0.11 valid=10s ipv6=off;
    include /etc/nginx/route.conf;
    map $http_upgrade $connection_upgrade { default upgrade; '' ''; }
    map $http_x_real_ip $client_ip { default $http_x_real_ip; '' $remote_addr; }
    server {
        listen 8080;
        client_max_body_size 256m;
        location / {
            proxy_pass http://sub2api_active;
            proxy_http_version 1.1;
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $client_ip;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $http_x_forwarded_proto;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection $connection_upgrade;
            proxy_connect_timeout 10s;
            proxy_read_timeout 900s;
            proxy_send_timeout 900s;
            proxy_buffering off;
            proxy_request_buffering off;
            proxy_cache off;
            proxy_next_upstream off;
            add_header X-Sub2-Slot $sub2api_slot always;
        }
    }
}
'''


def verify_router(slot):
    body, headers = fetch(f'http://127.0.0.1:{ROUTER_PORT}/health')
    if body.get('status') != 'ok' or headers.get('X-Sub2-Slot') != slot:
        raise RuntimeError('Router is not serving the expected healthy slot')


def wait_router(slot):
    for attempt in range(30):
        try:
            verify_router(slot)
            return
        except Exception:
            if attempt == 29:
                raise
            time.sleep(1)


def change_route(slot):
    health(slot)
    old = UPSTREAM.read_text()
    backup(UPSTREAM)
    try:
        atomic(UPSTREAM, route_text(slot), 0o644)
        command(['docker', 'exec', ROUTER, 'nginx', '-t'])
        command(['docker', 'exec', ROUTER, 'nginx', '-s', 'reload'])
        for attempt in range(10):
            try:
                verify_router(slot)
                return
            except Exception:
                if attempt == 9:
                    raise
                time.sleep(1)
    except Exception:
        atomic(UPSTREAM, old, 0o644)
        command(['docker', 'exec', ROUTER, 'nginx', '-t'])
        command(['docker', 'exec', ROUTER, 'nginx', '-s', 'reload'])
        raise


def connections(name):
    item = inspect(name)
    if not item['State']['Running']:
        return 0
    pid = item['State']['Pid']
    if not pid:
        raise RuntimeError('Cannot inspect app network namespace')
    # Host ss misses container-to-container connections (including NewAPI).
    raw = command(['nsenter', '-t', str(pid), '-n', 'ss', '-Hnt', 'state', 'established',
                   '( sport = :8080 )'])
    return len(raw.splitlines())


def wait_drain(name, seconds):
    deadline = time.monotonic() + seconds
    stable = 0
    while True:
        count = connections(name)
        stable = stable + 1 if count == 0 else 0
        if stable >= 3:
            return
        if time.monotonic() >= deadline:
            raise RuntimeError(f'{name} has {count} connections; retained without interruption; run finish later')
        time.sleep(3)


def stop_drained(name, seconds):
    wait_drain(name, seconds)
    command(['docker', 'stop', '--time', '180', name], 210)


def redirect_rules(old_ip, router_ip):
    import ipaddress
    ipaddress.IPv4Address(old_ip)
    ipaddress.IPv4Address(router_ip)
    match = ['-p', 'tcp', '--syn', '-d', old_ip + '/32', '--dport', '8080',
             '-m', 'comment', '--comment', 'sub2api-bootstrap']
    rules = [(chain, match + ['-j', 'DNAT', '--to-destination', router_ip + ':8080'])
             for chain in ['PREROUTING', 'OUTPUT']]
    rules.append(('POSTROUTING', ['-p', 'tcp', '-d', router_ip + '/32', '--dport', '8080',
                                 '-m', 'conntrack', '--ctorigdst', old_ip, '--ctorigdstport', '8080',
                                 '-m', 'comment', '--comment', 'sub2api-bootstrap', '-j', 'MASQUERADE']))
    return rules


def redirect_legacy(state):
    old = inspect(LEGACY)
    router = inspect(ROUTER)
    if old['Id'] != state['legacy_id']:
        raise RuntimeError('Legacy identity changed')
    rules = []
    for network in NETWORKS:
        rules += redirect_rules(old['NetworkSettings']['Networks'][network]['IPAddress'],
                                router['NetworkSettings']['Networks'][network]['IPAddress'])
    if state.get('redirect_namespace') == 'container' and state.get('redirect_rules') != [list(r) for r in rules]:
        raise RuntimeError('Bootstrap router address changed; inspect forwarding before proceeding')
    if state.get('redirect_namespace') != 'container':
        state['host_redirect_rules'] = state.get('redirect_rules', [])
    state['redirect_rules'] = rules
    state['redirect_namespace'] = 'container'
    save_state(state)
    # Bridge traffic need not traverse host iptables (br_netfilter is optional).
    # Redirect inside the legacy app's network namespace, without changing host
    # bridge sysctls. Match SYN only, preserving streams even if conntrack was not
    # enabled in this namespace before installation.
    namespace = ['nsenter', '-t', str(old['State']['Pid']), '-n']
    command(namespace + ['sysctl', '-w', 'net.ipv4.ip_forward=1'])
    for chain, rule in rules:
        check = subprocess.run(namespace + ['iptables', '-w', '10', '-t', 'nat', '-C', chain] + rule,
                               capture_output=True)
        if check.returncode == 1:
            command(namespace + ['iptables', '-w', '10', '-t', 'nat', '-I', chain, '1'] + rule)
        elif check.returncode:
            raise RuntimeError('Cannot verify bootstrap forwarding rule')


def clear_redirects(state):
    # The old container namespace (and its rules) disappears when it stops.
    # Remove any host rules left by the initial installation as well.
    for chain, rule in state.get('host_redirect_rules', []):
        check = subprocess.run(['iptables', '-w', '10', '-t', 'nat', '-C', chain] + rule,
                               capture_output=True)
        if check.returncode == 0:
            command(['iptables', '-w', '10', '-t', 'nat', '-D', chain] + rule)
        elif check.returncode != 1:
            raise RuntimeError('Cannot verify bootstrap forwarding cleanup')


def up(name):
    command(['docker', 'compose', '-f', str(COMPOSE), 'config', '-q'])
    command(['docker', 'compose', '-f', str(COMPOSE), 'up', '-d', '--no-deps', name], 180)


def wait_health(slot, image):
    deadline = time.monotonic() + 180
    while True:
        try:
            result = health(slot)
            if result['image'] != image:
                raise RuntimeError('App image identity mismatch')
            return result
        except Exception:
            if time.monotonic() >= deadline:
                raise
            time.sleep(3)


def replace_slot(slot, image, seconds):
    if slot == route():
        raise RuntimeError('Cannot replace the active customer slot')
    if slot_status(slot).get('created') is not False:
        wait_drain(SLOTS[slot][0], seconds)
    health(route())
    before = COMPOSE.read_text()
    backup(COMPOSE)
    data = json.loads(before)
    data['services'][SLOTS[slot][0]]['image'] = image
    try:
        atomic(COMPOSE, json.dumps(data, indent=2) + '\n')
        up(SLOTS[slot][0])
        return wait_health(slot, image)
    except Exception:
        atomic(COMPOSE, before)
        up(SLOTS[slot][0])
        raise


def bootstrap(image, router_image, baseline):
    if COMPOSE.exists() or STATE.exists():
        raise RuntimeError('Blue/green already initialized; use deploy or finish')
    if not re.fullmatch(r'nginx@sha256:[0-9a-f]{64}', router_image or ''):
        raise RuntimeError('Supply the verified nginx@sha256:digest router image')
    legacy = inspect(LEGACY)
    if not legacy['State']['Running']:
        raise RuntimeError('Legacy Sub2API must remain running during bootstrap')
    source = json.loads(LEGACY_COMPOSE.read_text())
    service = source['services'][LEGACY]
    if service['image'] != legacy['Config']['Image']:
        raise RuntimeError('Legacy manifest does not match the running app')
    if not set(NETWORKS).issubset(service['networks']):
        raise RuntimeError('Legacy network layout changed; inspect before bootstrapping')
    if 'proxy_pass http://127.0.0.1:8080;' not in SITE.read_text():
        raise RuntimeError('Unexpected external Sub2API route')
    backup(LEGACY_COMPOSE)
    backup(SITE)
    services = {}
    for slot, (name, port) in SLOTS.items():
        new = copy.deepcopy(service)
        new['image'] = image
        new['container_name'] = name
        new['networks'] = {network: {k: v for k, v in (options or {}).items() if k != 'aliases'}
                           for network, options in service['networks'].items()}
        new['ports'] = [f'127.0.0.1:{port}:8080']
        new['stop_grace_period'] = '180s'
        if slot != 'blue':
            new['profiles'] = ['standby']
        services[name] = new
    ROUTER_DIR.mkdir(mode=0o755)
    atomic(ROUTER_DIR / 'nginx.conf', ROUTER_CONFIG, 0o644)
    atomic(UPSTREAM, route_text('blue'), 0o644)
    services[ROUTER] = {'image': router_image, 'container_name': ROUTER, 'restart': 'unless-stopped',
                        'ports': [f'127.0.0.1:{ROUTER_PORT}:8080'],
                        'networks': {n: {'aliases': ['yoshub-sub2api', 'sub2api']} for n in NETWORKS},
                        'volumes': [f'{ROUTER_DIR}:/etc/nginx:ro'], 'stop_signal': 'SIGQUIT',
                        'stop_grace_period': '900s',
                        'healthcheck': {'test': ['CMD', 'wget', '-q', '-T', '5', '-O', '/dev/null',
                                                'http://127.0.0.1:8080/health'], 'interval': '15s',
                                        'timeout': '10s', 'retries': 3},
                        'security_opt': ['no-new-privileges:true']}
    manifest = {'name': 'yoshub-sub2api-bluegreen', 'services': services,
                'networks': {n: {'external': True, 'name': n} for n in NETWORKS}}
    atomic(COMPOSE, json.dumps(manifest, indent=2) + '\n')
    save_state({'pending': image, 'bootstrap': True, 'target': 'blue', 'old': 'legacy',
                'baseline': baseline, 'legacy_id': legacy['Id']})


def finish_bootstrap(state, seconds):
    image = state['pending']
    up(SLOTS['blue'][0])
    wait_health('blue', image)
    up(ROUTER)
    wait_router('blue')
    if inspect(LEGACY)['State']['Running']:
        redirect_legacy(state)
    old_site = SITE.read_text()
    new_site = old_site.replace('proxy_pass http://127.0.0.1:8080;',
                                f'proxy_pass http://127.0.0.1:{ROUTER_PORT};')
    if new_site != old_site:
        try:
            atomic(SITE, new_site, 0o644)
            command(['nginx', '-t'])
            command(['systemctl', 'reload', 'nginx'])
            external_health('blue')
        except Exception:
            atomic(SITE, old_site, 0o644)
            command(['nginx', '-t'])
            command(['systemctl', 'reload', 'nginx'])
            raise
    external_health('blue')
    if inspect(LEGACY)['Id'] != state['legacy_id']:
        raise RuntimeError('Legacy app changed during bootstrap; refusing to stop it')
    # During first installation Docker DNS can still resolve the legacy alias.
    # Wait for all its internal AND external sockets, including idle keepalives.
    # No timeout path stops this app. Future releases only reload the stable router.
    stop_drained(LEGACY, seconds)
    clear_redirects(state)
    data = json.loads(LEGACY_COMPOSE.read_text())
    if LEGACY in data['services']:
        backup(LEGACY_COMPOSE)
        del data['services'][LEGACY]
        includes = data.setdefault('include', [])
        if not any(isinstance(item, dict) and item.get('path') == str(COMPOSE) for item in includes):
            includes.append({'path': str(COMPOSE)})
        atomic(LEGACY_COMPOSE, json.dumps(data, indent=2) + '\n')
        command(['docker', 'compose', '-f', str(LEGACY_COMPOSE), 'config', '-q'])
    # Keep the standby stopped: Sub2API has no NewAPI-style master/slave role.
    # Brief overlap for cutover is enough; do not run two sets of background jobs forever.
    stale = ROOT / 'yoshub-sub2api.compose.json'
    if stale.exists():
        backup(stale)
        atomic(stale, json.dumps({'include': [{'path': str(COMPOSE)}]}, indent=2) + '\n')
    return 'blue'


def external_health(slot):
    raw = command(['curl', '--noproxy', '*', '--resolve', 'sub2.yoshub.com:443:127.0.0.1',
                   '-fsS', '--max-time', '10', 'https://sub2.yoshub.com/health'])
    if json.loads(raw).get('status') != 'ok':
        raise RuntimeError('External Sub2API route health failed')
    verify_router(slot)


def finish(image, seconds):
    state = json.loads(STATE.read_text())
    if state.get('pending') != image:
        raise RuntimeError('Pending deployment image does not match')
    if state.get('bootstrap'):
        active = finish_bootstrap(state, seconds)
    else:
        active = state['target']
        wait_health(active, image)
        if route() != active:
            change_route(active)
        external_health(active)
        stop_drained(SLOTS[state['old']][0], seconds)
    if infrastructure() != state['baseline']:
        raise RuntimeError('Infrastructure identities changed')
    external_health(active)
    manifest = json.loads(COMPOSE.read_text())
    for slot, (name, _) in SLOTS.items():
        if slot == active:
            manifest['services'][name].pop('profiles', None)
        else:
            manifest['services'][name]['profiles'] = ['standby']
    atomic(COMPOSE, json.dumps(manifest, indent=2) + '\n')
    save_state({'pending': None, 'active_slot': active, 'active_image': image})
    return {'deployed': image, 'active_slot': active, 'slots': [slot_status(s) for s in SLOTS]}


def main():
    import fcntl
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['status', 'bootstrap', 'deploy', 'finish', 'rollback'])
    parser.add_argument('target', nargs='?')
    parser.add_argument('--router-image')
    parser.add_argument('--timeout', type=int, default=300)
    args = parser.parse_args()
    if os.geteuid() != 0 or not ROOT.is_dir():
        raise RuntimeError('Run with sudo -n on dedicated-YosHub')
    with (ROOT / 'operation.lock').open('w') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        baseline = infrastructure()
        try:
            if args.action == 'status':
                result = {'active_slot': route(), 'state': json.loads(STATE.read_text()),
                          'slots': [slot_status(s) for s in SLOTS], 'infrastructure': baseline}
            elif args.action == 'rollback':
                if args.target not in SLOTS or args.target == route():
                    raise RuntimeError('Choose the inactive slot')
                old = route()
                up(SLOTS[args.target][0])
                previous = json.loads(COMPOSE.read_text())['services'][SLOTS[args.target][0]]['image']
                wait_health(args.target, previous)
                save_state({'pending': previous, 'target': args.target, 'old': old, 'baseline': baseline})
                change_route(args.target)
                result = finish(previous, args.timeout)
            else:
                validate_image(args.target)
                if args.action != 'finish':
                    if STATE.exists() and json.loads(STATE.read_text()).get('pending'):
                        raise RuntimeError('Finish the pending release first')
                    command(['runuser', '-u', 'debian', '--', 'docker', 'pull', args.target], 600)
                    if args.action == 'bootstrap':
                        command(['runuser', '-u', 'debian', '--', 'docker', 'pull', args.router_image], 180)
                        bootstrap(args.target, args.router_image, baseline)
                    else:
                        old = route()
                        target = 'green' if old == 'blue' else 'blue'
                        replace_slot(target, args.target, args.timeout)
                        save_state({'pending': args.target, 'target': target, 'old': old, 'baseline': baseline})
                        change_route(target)
                result = finish(args.target, args.timeout)
            print(json.dumps(result, ensure_ascii=False))
        finally:
            if infrastructure() != baseline:
                raise RuntimeError('Infrastructure identities changed during operation')


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        print('ERROR: ' + str(exc), file=sys.stderr)
        sys.exit(1)
