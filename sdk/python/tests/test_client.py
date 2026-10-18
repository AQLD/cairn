"""Tests for the Python client against a real engine.

    python3 -m unittest discover -s sdk/python/tests

Uses the engine at CAIRN_ENDPOINT (and CAIRN_TOKEN) when set; otherwise builds
cairnd from this repository and starts it on the host runtime (no isolation:
the tests run only trusted commands).
"""

from __future__ import annotations

import http.server
import io
import json
import os
import pathlib
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import unittest
import urllib.request

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))

import cairn  # noqa: E402

REPO = HERE.parents[2]


class Engine:
    """The engine under test, shared by every test in the run."""

    endpoint = ""
    token = ""
    own = False  # started by these tests (so restartable with other flags)
    _proc = None
    _dir = None
    _log = None

    @classmethod
    def start(cls):
        if cls.endpoint:
            return
        if os.environ.get("CAIRN_ENDPOINT"):
            cls.endpoint, cls.token = os.environ["CAIRN_ENDPOINT"], os.environ.get("CAIRN_TOKEN", "")
            return
        cls.own = True
        cls._dir = tempfile.mkdtemp(prefix="cairn-sdk-")
        binary = os.path.join(cls._dir, "cairnd")
        subprocess.run(["go", "build", "-o", binary, "./cmd/cairnd"], cwd=REPO, check=True,
                       env={**os.environ, "CGO_ENABLED": "0"})
        port = _free_port()
        cls.token = "sdk-test-token"
        cls._log = open(os.path.join(cls._dir, "cairnd.log"), "wb")
        cls._proc = subprocess.Popen(
            [binary, "--runtime", "host", "--data", os.path.join(cls._dir, "data"),
             "--listen", f"127.0.0.1:{port}", "--token", cls.token],
            stdout=subprocess.DEVNULL, stderr=cls._log)
        cls.endpoint = f"http://127.0.0.1:{port}"
        deadline = time.time() + 30
        while time.time() < deadline:
            try:
                urllib.request.urlopen(cls.endpoint + "/healthz", timeout=1)
                return
            except OSError:
                time.sleep(0.1)
        raise RuntimeError("cairnd did not start: " + open(os.path.join(cls._dir, "cairnd.log")).read())

    @classmethod
    def stop(cls):
        if cls._proc:
            cls._proc.terminate()
            cls._proc.wait(timeout=30)
        if cls._log:
            cls._log.close()
        if cls._dir:
            shutil.rmtree(cls._dir, ignore_errors=True)


def setUpModule():
    Engine.start()


def tearDownModule():
    Engine.stop()


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class EngineTest(unittest.TestCase):
    def setUp(self):
        self.c = cairn.Client(Engine.endpoint, Engine.token)
        self.info = self.c.info()

    def workspace(self, name="sdk", **kw) -> cairn.Workspace:
        ws = self.c.create_workspace(name, **kw)
        self.addCleanup(self._delete, ws.id)
        return ws

    def _delete(self, ws_id):
        try:
            self.c.delete_workspace(ws_id)
        except cairn.NotFound:
            pass


class TestEngine(EngineTest):
    def test_info_health_metrics(self):
        self.c.health()
        self.assertTrue(self.info.runtime)
        self.assertGreaterEqual(self.info.save_format, 1)
        self.assertIn("cairn_", self.c.metrics())


class TestWorkspaces(EngineTest):
    def test_create_get_list_delete(self):
        ws = self.workspace("listed")
        self.assertEqual(ws.name, "listed")
        self.assertEqual(ws.state, "running")
        self.assertEqual(self.c.workspace(ws.id).id, ws.id)
        self.assertIn(ws.id, [w.id for w in self.c.workspaces()])
        ws.delete()
        with self.assertRaises(cairn.NotFound) as e:
            self.c.workspace(ws.id)
        self.assertEqual(e.exception.code, "not_found")
        self.assertEqual(e.exception.status, 404)
        self.assertTrue(e.exception.request_id, "errors carry the request id the engine logged")

    def test_retried_create_returns_the_same_workspace(self):
        a = self.workspace("once", idempotency_key="sdk-create-key-1")
        b = self.c.create_workspace("once", idempotency_key="sdk-create-key-1")
        self.assertEqual(a.id, b.id)


class TestCommands(EngineTest):
    def setUp(self):
        super().setUp()
        self.ws = self.workspace("commands")

    def test_exit_code_and_separate_streams(self):
        r = self.ws.exec("echo out; echo err >&2; exit 3")
        self.assertEqual(r.exit_code, 3)
        self.assertFalse(r.ok)
        self.assertEqual(r.stdout, b"out\n")
        self.assertEqual(r.stderr, b"err\n")

    def test_stdin_env_and_working_folder(self):
        self.ws.run("mkdir", "-p", "sub")
        r = self.ws.exec(["sh", "-c", 'cat; echo "$GREETING"; pwd'], stdin="from stdin\n",
                         env={"GREETING": "hello"}, cwd="sub")
        lines = r.text.splitlines()
        self.assertEqual(lines[:2], ["from stdin", "hello"])
        self.assertTrue(lines[2].endswith("/sub"), lines[2])

    def test_timeout_marks_the_command(self):
        r = self.ws.exec(["sleep", "30"], timeout=0.5)
        self.assertTrue(r.timed_out)
        self.assertLess(r.duration_ms, 15000)

    def test_unknown_command_exits_127(self):
        self.assertEqual(self.ws.exec(["no-such-command-xyz"]).exit_code, 127)

    def test_binary_output_is_exact(self):
        r = self.ws.exec(["sh", "-c", r"printf '\000\377\200abc'"])
        self.assertEqual(r.stdout, b"\x00\xff\x80abc")

    def test_run_returns_text_and_raises_on_failure(self):
        self.assertEqual(self.ws.run("echo", "hi"), "hi\n")
        with self.assertRaises(cairn.CommandFailed) as e:
            self.ws.run("sh -c 'echo nope >&2; exit 2'")
        self.assertEqual(e.exception.result.exit_code, 2)
        self.assertIn("nope", str(e.exception))

    def test_streamed_output_arrives_in_order_while_running(self):
        chunks = []
        start = time.time()
        r = self.ws.exec("for i in 1 2 3; do echo line$i; sleep 0.3; done; printf '\\377' >&2",
                         on_output=lambda stream, data: chunks.append((stream, data, time.time() - start)))
        self.assertEqual(r.exit_code, 0)
        out = b"".join(d for s, d, _ in chunks if s == "stdout")
        err = b"".join(d for s, d, _ in chunks if s == "stderr")
        self.assertEqual(out, b"line1\nline2\nline3\n")
        self.assertEqual(err, b"\xff")
        first = next(t for s, _, t in chunks if s == "stdout")
        self.assertLess(first, 0.5, "the first line arrives before the command ends")


class TestFiles(EngineTest):
    def setUp(self):
        super().setUp()
        self.ws = self.workspace("files")

    def test_write_and_read_every_kind_of_content(self):
        self.ws.write_file("a/bytes.bin", b"\x00\x01")
        self.ws.write_file("a/text.txt", "héllo")
        self.ws.write_file("a/stream.bin", io.BytesIO(b"from a file object"))
        with tempfile.NamedTemporaryFile(delete=False) as f:
            f.write(b"from a local path")
        self.addCleanup(os.unlink, f.name)
        self.ws.write_file("a/path.bin", pathlib.Path(f.name))
        self.assertEqual(self.ws.read_file("a/bytes.bin"), b"\x00\x01")
        self.assertEqual(self.ws.read_file("a/text.txt").decode(), "héllo")
        self.assertEqual(self.ws.read_file("a/stream.bin"), b"from a file object")
        self.assertEqual(self.ws.read_file("a/path.bin"), b"from a local path")

    def test_large_file_streams_both_ways(self):
        blob = os.urandom(1 << 20) * 8  # 8 MiB
        self.ws.write_file("big.bin", io.BytesIO(blob))
        out = io.BytesIO()
        self.assertEqual(self.c.download_file(self.ws.id, "big.bin", out), len(blob))
        self.assertEqual(out.getvalue(), blob)

    def test_paths_cannot_escape_the_workspace(self):
        # ".." stops at the workspace's root: the path is either refused or
        # means the workspace's own /etc/passwd, never the host's.
        self.ws.write_file("etc/passwd", "the workspace's own\n")
        try:
            got = self.ws.read_file("../../etc/passwd")
        except cairn.CairnError as e:
            self.assertIn(e.code, ("invalid_argument", "not_found"))
            return
        self.assertEqual(got, b"the workspace's own\n")

    def test_missing_file_is_not_found(self):
        with self.assertRaises(cairn.NotFound):
            self.ws.read_file("nope.txt")


class TestSaves(EngineTest):
    def test_resume_rolls_back_and_copies_are_independent(self):
        ws = self.workspace("saves")
        ws.write_file("note.txt", "saved")
        save = ws.save()
        self.assertEqual(save.kind, "quick")
        self.assertEqual(save.state, "ready")
        ws.write_file("note.txt", "changed")
        save.resume()
        self.assertEqual(ws.read_file("note.txt"), b"saved")
        self.assertIn(save.id, [s.id for s in ws.saves()])

        copies = save.copy(2)
        self.assertEqual(len(copies), 2)
        for cp in copies:
            self.addCleanup(self._delete, cp.id)
        copies[0].write_file("note.txt", "only in copy 0")
        self.assertEqual(copies[1].read_file("note.txt"), b"saved")
        self.assertEqual(ws.read_file("note.txt"), b"saved")

    def test_retried_save_and_copy_return_the_first_result(self):
        ws = self.workspace("retries")
        a = ws.save(idempotency_key="sdk-save-key-1")
        b = ws.save(idempotency_key="sdk-save-key-1")
        self.assertEqual(a.id, b.id)
        c1 = a.copy(1, idempotency_key="sdk-copy-key-1")
        c2 = a.copy(1, idempotency_key="sdk-copy-key-1")
        self.addCleanup(self._delete, c1[0].id)
        self.assertEqual([w.id for w in c1], [w.id for w in c2])

    def test_delete_undelete_and_verify(self):
        ws = self.workspace("lifecycle")
        save = ws.save()
        save.delete()
        self.assertEqual(self.c.get_save(save.id).state, "deleted")
        self.assertEqual(self.c.undelete_save(save.id).state, "ready")
        self.assertEqual(save.verify().state, "ready")

    def test_full_save_keeps_a_running_process(self):
        if not (self.info.capabilities.full_save and self.info.capabilities.persistent_processes):
            self.skipTest(f"runtime {self.info.runtime} cannot save memory")
        ws = self.workspace("full")
        ws.exec("nohup sh -c 'echo $$ > /tmp/pid; while :; do sleep 1; done' >/dev/null 2>&1 &")
        time.sleep(0.5)
        pid = ws.read_file("tmp/pid")
        save = ws.save("full")
        self.assertEqual(self.c.workspace(ws.id).state, "stopped")
        save.resume()
        self.assertEqual(ws.run("cat", "/tmp/pid").encode(), pid)
        self.assertEqual(ws.exec(f"kill -0 {pid.decode().strip()}").exit_code, 0, "the process is still alive")

    def test_pause_frees_memory_or_says_it_cannot(self):
        ws = self.workspace("pause")
        if not self.info.capabilities.full_save:
            with self.assertRaises(cairn.UnsupportedByRuntime):
                ws.pause()
            return
        self.assertEqual(ws.pause().state, "paused")
        self.assertEqual(ws.run("echo", "awake"), "awake\n")


class TestTemplates(EngineTest):
    def test_import_use_and_delete(self):
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w") as tar:
            data = b"from the template\n"
            info = tarfile.TarInfo("etc/motd")
            info.size = len(data)
            tar.addfile(info, io.BytesIO(data))
        buf.seek(0)
        name = f"sdk-{os.getpid()}"
        t = self.c.import_template(name, buf)
        self.addCleanup(lambda: self.c.delete_template(name))
        self.assertEqual(t.name, name)
        self.assertIn(name, [x.name for x in self.c.templates()])
        ws = self.workspace("templated", template=name)
        self.assertEqual(ws.read_file("etc/motd"), b"from the template\n")


class TestAuth(unittest.TestCase):
    def test_a_wrong_token_is_refused(self):
        if not Engine.token:
            self.skipTest("the engine under test has no token")
        with self.assertRaises(cairn.Unauthorized):
            cairn.Client(Engine.endpoint, "wrong-token").workspaces()


# -- retries, against a fake engine -------------------------------------------


class FakeEngine(http.server.ThreadingHTTPServer):
    """Answers with a scripted list of actions: "drop" closes the connection
    without answering, "busy" answers 429, anything else is JSON sent as 200."""

    def __init__(self, script):
        self.script = list(script)
        self.seen = []  # (method, path, Idempotency-Key) per request
        super().__init__(("127.0.0.1", 0), _FakeHandler)
        threading.Thread(target=self.serve_forever, daemon=True).start()

    @property
    def url(self):
        return f"http://127.0.0.1:{self.server_address[1]}"


class _FakeHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _handle(self):
        n = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(n)
        self.server.seen.append((self.command, self.path, self.headers.get("Idempotency-Key")))
        action = self.server.script.pop(0) if self.server.script else {}
        if action == "drop":
            self.close_connection = True
            self.connection.shutdown(socket.SHUT_RDWR)
            return
        if action == "busy":
            body = b'{"error":{"code":"too_many_requests","message":"busy"}}'
            self.send_response(429)
            self.send_header("Retry-After", "0")
        else:
            body = json.dumps(action).encode()
            self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_GET = do_POST = do_PUT = do_DELETE = _handle


WS = {"id": "ws_1", "name": "x", "state": "running", "limits": {}}


class TestRetries(unittest.TestCase):
    def fake(self, script):
        f = FakeEngine(script)
        self.addCleanup(f.server_close)
        self.addCleanup(f.shutdown)
        return f, cairn.Client(f.url, "t", retries=3)

    def test_create_after_a_dropped_connection_retries_with_the_same_key(self):
        f, c = self.fake(["drop", WS])
        self.assertEqual(c.create_workspace("x").id, "ws_1")
        keys = [k for _, _, k in f.seen]
        self.assertEqual(len(keys), 2)
        self.assertTrue(keys[0])
        self.assertEqual(keys[0], keys[1], "the retry must reuse the key so the engine can deduplicate it")

    def test_each_call_gets_its_own_key(self):
        f, c = self.fake([WS, WS])
        c.create_workspace("a")
        c.create_workspace("b")
        self.assertNotEqual(f.seen[0][2], f.seen[1][2])

    def test_a_busy_engine_is_retried_even_for_commands(self):
        f, c = self.fake(["busy", {"exit_code": 0, "stdout": "ok", "stderr": "", "duration_ms": 1}])
        self.assertEqual(c.exec("ws_1", ["true"]).stdout, b"ok")
        self.assertEqual(len(f.seen), 2)

    def test_a_command_is_never_retried_after_a_dropped_connection(self):
        f, c = self.fake(["drop", {"exit_code": 0}])
        with self.assertRaises(cairn.ConnectionFailed):
            c.exec("ws_1", ["rm", "-rf", "build"])
        self.assertEqual(len(f.seen), 1, "running a command twice is not safe")

    def test_resume_is_not_retried_after_a_dropped_connection(self):
        f, c = self.fake(["drop", WS])
        with self.assertRaises(cairn.ConnectionFailed):
            c.resume("sv_1")
        self.assertEqual(len(f.seen), 1, "a second resume could roll back work done since the first")

    def test_reads_are_retried(self):
        f, c = self.fake(["drop", "busy", WS])
        self.assertEqual(c.workspace("ws_1").id, "ws_1")
        self.assertEqual(len(f.seen), 3)

    def test_gives_up_after_the_retry_budget(self):
        f, c = self.fake(["drop"] * 10)
        with self.assertRaises(cairn.ConnectionFailed):
            c.workspaces()
        self.assertEqual(len(f.seen), 4)  # one try and three retries

    def test_an_unreachable_engine_is_a_connection_error(self):
        c = cairn.Client(f"http://127.0.0.1:{_free_port()}", retries=0)
        with self.assertRaises(cairn.ConnectionFailed):
            c.health()


if __name__ == "__main__":
    unittest.main()
