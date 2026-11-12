"""Real Docker/Nginx SSE and WebSocket cutover test; uses only isolated fixtures."""
import importlib.util
import json
import os
import pathlib
import socket
import subprocess
import tempfile
import time
import urllib.request
import uuid

spec = importlib.util.spec_from_file_location('operate', pathlib.Path(__file__).with_name('sub2api-operate.py'))
op = importlib.util.module_from_spec(spec)
spec.loader.exec_module(op)

SERVER = r'''
import base64,hashlib,json,os,time
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
class Handler(BaseHTTPRequestHandler):
    protocol_version='HTTP/1.1'
    def log_message(self,*args): pass
    def do_GET(self):
        slot=os.environ['SLOT']
        if self.path=='/ws':
            key=self.headers['Sec-WebSocket-Key']+'258EAFA5-E914-47DA-95CA-C5AB0DC85B11'
            self.send_response(101); self.send_header('Upgrade','websocket'); self.send_header('Connection','Upgrade')
            self.send_header('Sec-WebSocket-Accept',base64.b64encode(hashlib.sha1(key.encode()).digest()).decode()); self.end_headers()
            for i in range(20):
                b=(slot+':'+str(i)).encode(); self.wfile.write(bytes([129,len(b)])+b); self.wfile.flush(); time.sleep(.12)
            self.wfile.write(bytes([136,0])); self.wfile.flush(); self.close_connection=True
            return
        if self.path=='/stream':
            self.send_response(200); self.send_header('Content-Type','text/event-stream'); self.send_header('Connection','close'); self.end_headers()
            for i in range(20):
                self.wfile.write(('data: '+slot+':'+str(i)+'\n\n').encode()); self.wfile.flush(); time.sleep(.12)
            self.close_connection=True
            return
        b=json.dumps({'status':'ok','slot':slot}).encode()
        self.send_response(200); self.send_header('Content-Length',str(len(b))); self.end_headers(); self.wfile.write(b)
ThreadingHTTPServer(('0.0.0.0',8080),Handler).serve_forever()
'''


def run(*args):
    try:
        return subprocess.check_output(args, stderr=subprocess.STDOUT).decode().strip()
    except subprocess.CalledProcessError as exc:
        raise RuntimeError(f'Fixture command failed: {args!r}\n{exc.output.decode(errors="replace")}') from exc


def main():
    task = 'sub2-cutover-' + uuid.uuid4().hex[:8]
    containers = []
    redirects = []
    firewall = []
    run('docker', 'network', 'create', task)
    try:
        with tempfile.TemporaryDirectory(prefix=task) as directory:
            root = pathlib.Path(directory)
            (root / 'server.py').write_text(SERVER)
            (root / 'nginx.conf').write_text(op.ROUTER_CONFIG)
            (root / 'route.conf').write_text(op.route_text('blue'))
            for slot in ['blue', 'green']:
                name = task + '-' + slot
                containers.append(name)
                run('docker', 'run', '-d', '--name', name, '--network', task, '--network-alias', 'sub2api-' + slot,
                    '-p', '127.0.0.1::8080', '-v', f'{root}:/fixture:ro', '-e', 'SLOT=' + slot, 'python:3.13-alpine', 'python', '/fixture/server.py')
            name = task + '-router'
            containers.append(name)
            run('docker', 'run', '-d', '--name', name, '--network', task, '-p', '127.0.0.1::8080',
                '-v', f'{root}:/etc/nginx:ro', 'nginx:1.28-alpine')
            port = json.loads(run('docker', 'inspect', name))[0]['NetworkSettings']['Ports']['8080/tcp'][0]['HostPort']
            url = 'http://127.0.0.1:' + port
            for _ in range(40):
                try:
                    if json.load(urllib.request.urlopen(url + '/health'))['slot'] == 'blue':
                        break
                except Exception:
                    time.sleep(.1)
            else:
                raise RuntimeError('Fixture failed to start')
            sse = urllib.request.urlopen(url + '/stream', timeout=10)
            assert sse.readline() == b'data: blue:0\n'
            ws = socket.create_connection(('127.0.0.1', int(port)), timeout=10)
            ws.sendall(b'GET /ws HTTP/1.1\r\nHost: fixture\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n')
            reader = ws.makefile('rb')
            assert b'101' in reader.readline()
            while reader.readline() != b'\r\n':
                pass
            assert reader.read(2) == bytes([129, 6])
            assert reader.read(6) == b'blue:0'
            (root / 'route.conf').write_text(op.route_text('green'))
            run('docker', 'exec', name, 'nginx', '-t')
            run('docker', 'exec', name, 'nginx', '-s', 'reload')
            for _ in range(30):
                if json.load(urllib.request.urlopen(url + '/health'))['slot'] == 'green':
                    break
                time.sleep(.1)
            else:
                raise RuntimeError('New connections did not switch')
            sse_rest = sse.read().decode()
            assert 'blue:19' in sse_rest and 'green:' not in sse_rest
            messages = []
            while True:
                opcode, length = reader.read(2)
                if opcode == 136:
                    break
                messages.append(reader.read(length).decode())
            assert messages == ['blue:' + str(i) for i in range(1, 20)], messages
            ws.close()
            assert json.load(urllib.request.urlopen(url + '/health'))['slot'] == 'green'
            old = json.loads(run('docker', 'inspect', containers[0]))[0]
            old_ip = old['NetworkSettings']['Networks'][task]['IPAddress']
            old_port = old['NetworkSettings']['Ports']['8080/tcp'][0]['HostPort']
            legacy_stream = urllib.request.urlopen('http://127.0.0.1:' + old_port + '/stream', timeout=10)
            assert legacy_stream.readline() == b'data: blue:0\n'
            helper_image = os.environ.get('SUB2API_CUTOVER_HELPER_IMAGE', 'alpine:3.21')
            firewall = ['docker', 'run', '--rm', '--privileged', '--pid', 'host', helper_image,
                        'nsenter', '-t', '1', '-m', '-n', 'nsenter', '-t', str(old['State']['Pid']), '-n',
                        'iptables', '-w', '10', '-t', 'nat']
            router_ip = json.loads(run('docker', 'inspect', name))[0]['NetworkSettings']['Networks'][task]['IPAddress']
            for chain, rule in op.redirect_rules(old_ip, router_ip):
                run(*(firewall + ['-I', chain, '1'] + rule))
                redirects.append((chain, rule))
            # Even a client with the legacy IP cached must reach the new router.
            result = run('docker', 'run', '--rm', '--network', task, 'python:3.13-alpine', 'python', '-c',
                         f'import urllib.request; print(urllib.request.urlopen("http://{old_ip}:8080/health").read().decode())')
            assert json.loads(result)['slot'] == 'green', result
            legacy_rest = legacy_stream.read().decode()
            assert 'blue:19' in legacy_rest and 'green:' not in legacy_rest
            print(json.dumps({'sse_frames': 20, 'websocket_frames': 20, 'old_streams_completed': True,
                              'new_connections_slot': 'green', 'cached_legacy_ip_forwarded': True,
                              'legacy_existing_stream_survived': True}))
    finally:
        for chain, rule in reversed(redirects):
            subprocess.run(firewall + ['-D', chain] + rule, capture_output=True)
        for name in reversed(containers):
            subprocess.run(['docker', 'rm', '-f', name], capture_output=True)
        subprocess.run(['docker', 'network', 'rm', task], capture_output=True)


if __name__ == '__main__':
    main()
