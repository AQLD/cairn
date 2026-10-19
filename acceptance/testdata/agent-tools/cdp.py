# Drive the headless browser's first page over the Chrome DevTools Protocol,
# the way browser agents do, with only the standard library:
#   cdp.py goto URL            open URL and wait for it to load
#   cdp.py eval JS             print the value of a JavaScript expression (JSON)
#   cdp.py submit SEL PATH     submit the form at selector SEL, wait for PATH
#   cdp.py shot FILE           save a screenshot (PNG)
# Each run is its own DevTools connection, as each agent or person would be.
import base64
import json
import os
import socket
import struct
import sys
import time
import urllib.parse
import urllib.request


def page_url():
    for _ in range(100):
        try:
            with urllib.request.urlopen('http://127.0.0.1:9222/json/list', timeout=5) as r:
                pages = [t for t in json.load(r) if t['type'] == 'page']
            if pages:
                return pages[0]['webSocketDebuggerUrl']
        except OSError:
            pass
        time.sleep(0.1)
    sys.exit('no browser page on 127.0.0.1:9222')


class DevTools:
    def __init__(self, url):
        hostport, path = url[len('ws://'):].split('/', 1)
        host, port = hostport.rsplit(':', 1)
        self.s = socket.create_connection((host, int(port)), timeout=30)
        self.buf, self.id = b'', 0
        key = base64.b64encode(os.urandom(16)).decode()
        self.s.sendall(('GET /%s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n'
                        'Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n' % (path, hostport, key)).encode())
        while b'\r\n\r\n' not in self.buf:
            self.fill()
        head, self.buf = self.buf.split(b'\r\n\r\n', 1)
        if b' 101 ' not in head.split(b'\r\n')[0]:
            sys.exit('DevTools refused the connection: %s' % head.decode(errors='replace'))

    def fill(self):
        b = self.s.recv(1 << 16)
        if not b:
            raise EOFError('DevTools closed the connection')
        self.buf += b

    def take(self, n):
        while len(self.buf) < n:
            self.fill()
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def send(self, msg):
        data, mask = json.dumps(msg).encode(), os.urandom(4)
        n = len(data)
        if n < 126:
            head = struct.pack('>BB', 0x81, 0x80 | n)
        elif n < 1 << 16:
            head = struct.pack('>BBH', 0x81, 0x80 | 126, n)
        else:
            head = struct.pack('>BBQ', 0x81, 0x80 | 127, n)
        self.s.sendall(head + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))

    def recv(self):
        msg = b''
        while True:
            b0, b1 = self.take(2)
            n = b1 & 0x7f
            if n == 126:
                n = struct.unpack('>H', self.take(2))[0]
            elif n == 127:
                n = struct.unpack('>Q', self.take(8))[0]
            payload, op = self.take(n), b0 & 0x0f
            if op == 8:
                raise EOFError('DevTools closed the connection')
            if op in (0, 1, 2):
                msg += payload
                if b0 & 0x80:
                    return json.loads(msg)

    def call(self, method, **params):
        self.id += 1
        self.send({'id': self.id, 'method': method, 'params': params})
        while True:
            m = self.recv()
            if m.get('id') == self.id:
                if 'error' in m:
                    sys.exit('%s: %s' % (method, m['error']))
                return m['result']

    def eval(self, js):
        r = self.call('Runtime.evaluate', expression=js, awaitPromise=True, returnByValue=True)
        if 'exceptionDetails' in r:
            sys.exit('%s: %s' % (js, r['exceptionDetails']))
        return r['result'].get('value')

    def wait(self, path):
        for _ in range(300):
            try:
                if self.eval('location.pathname + " " + document.readyState') == path + ' complete':
                    return
            except SystemExit:
                pass  # the page is between documents
            time.sleep(0.1)
        sys.exit('page did not reach %s: at %s' % (path, self.eval('location.href')))


def main():
    cmd, args = sys.argv[1], sys.argv[2:]
    d = DevTools(page_url())
    if cmd == 'goto':
        d.call('Page.navigate', url=args[0])
        d.wait(urllib.parse.urlparse(args[0]).path or '/')
    elif cmd == 'eval':
        print(json.dumps(d.eval(args[0])))
    elif cmd == 'submit':
        d.eval('document.querySelector(%s).requestSubmit()' % json.dumps(args[0]))
        d.wait(args[1])
    elif cmd == 'shot':
        png = base64.b64decode(d.call('Page.captureScreenshot', format='png')['data'])
        with open(args[0], 'wb') as f:
            f.write(png)
    else:
        sys.exit('unknown command ' + cmd)


main()
