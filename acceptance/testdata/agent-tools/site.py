# A web app with a two-step sign-in, served on 127.0.0.1:8000: a password,
# then a one-time code "sent" to the person (written to /root/outbox/code).
# Pending sign-ins and sessions live only in this process's memory, and the
# session cookie only in the browser's, so a sign-in survives a pause only if
# both processes do.
import http.server
import os
import secrets
import urllib.parse

PASSWORD = 'correct horse'
pending = {}   # pending sign-in id -> (user, code)
sessions = {}  # session id -> user
issued = 0


def page(body):
    return ('<!doctype html><meta charset=utf-8><title>Cairn test app</title>'
            '<body style="font: 20px sans-serif; margin: 40px">' + body + '</body>').encode()


class App(http.server.BaseHTTPRequestHandler):
    def cookie(self, name):
        for part in self.headers.get('Cookie', '').split(';'):
            k, _, v = part.strip().partition('=')
            if k == name:
                return v
        return None

    def form(self):
        n = int(self.headers.get('Content-Length', 0))
        return {k: v[0] for k, v in urllib.parse.parse_qs(self.rfile.read(n).decode()).items()}

    def send(self, body, status=200, headers=()):
        self.send_response(status)
        for k, v in headers:
            self.send_header(k, v)
        self.send_header('Content-Type', 'text/html; charset=utf-8')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def redirect(self, to, headers=()):
        self.send(b'', 303, [('Location', to), *headers])

    def user(self):
        return sessions.get(self.cookie('session'))

    def do_GET(self):
        if self.path == '/login':
            self.send(page('<h1>Sign in</h1><form method=post action=/login>'
                           '<input name=user> <input name=password type=password> <button>Sign in</button></form>'))
        elif self.path == '/verify':
            self.send(page('<h1>Enter the code we sent you</h1><form method=post action=/verify>'
                           '<input name=code> <button>Verify</button></form>'))
        elif self.path == '/account':
            user = self.user()
            if not user:
                return self.redirect('/login')
            self.send(page('<h1>Signed in as %s</h1><p>Refund #1042 is waiting for approval.</p>'
                           '<form method=post action=/approve><button id=approve>Approve refund</button></form>' % user))
        else:
            self.redirect('/account')

    def do_POST(self):
        global issued
        f = self.form()
        if self.path == '/login':
            if f.get('password') != PASSWORD:
                return self.send(page('<h1>Wrong password</h1>'), 403)
            pid, code = secrets.token_hex(8), '%06d' % secrets.randbelow(10**6)
            pending[pid] = (f.get('user', ''), code)
            issued += 1
            with open('/root/outbox/code.tmp', 'w') as out:
                out.write('%s %d\n' % (code, issued))
            os.replace('/root/outbox/code.tmp', '/root/outbox/code')
            self.redirect('/verify', [('Set-Cookie', 'pending=%s; Path=/; HttpOnly' % pid)])
        elif self.path == '/verify':
            user, code = pending.get(self.cookie('pending'), (None, None))
            if not code or f.get('code') != code:
                return self.send(page('<h1>Wrong code</h1>'), 403)
            del pending[self.cookie('pending')]
            sid = secrets.token_hex(16)
            sessions[sid] = user
            # no Expires: a session cookie, kept only in the browser's memory
            self.redirect('/account', [('Set-Cookie', 'session=%s; Path=/; HttpOnly' % sid)])
        elif self.path == '/approve':
            user = self.user()
            if not user:
                return self.send(page('<h1>Not signed in</h1>'), 401)
            with open('/root/approvals.log', 'a') as out:
                out.write('refund 1042 approved by %s\n' % user)
            self.send(page('<h1>Refund #1042 approved by %s</h1>' % user))

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(('127.0.0.1', 8000), App).serve_forever()
