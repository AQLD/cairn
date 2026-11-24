"""The Cairn client: one method per API operation, standard library only."""

from __future__ import annotations

import base64
import fnmatch
import io
import json
import os
import posixpath
import random
import shutil
import socket
import ssl
import stat
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import IO, Any, Iterable, Iterator, Optional, Sequence, Union

from . import __version__
from ._errors import CairnError, CommandFailed, ConnectionFailed, TooManyRequests, from_response
from ._types import Event, ExecResult, Info, Limits, OutputCallback, RunResult, Save, SaveDiff, Session, Template, Workspace

Content = Union[bytes, str, IO[bytes], "os.PathLike[str]"]

# Methods the engine treats as safe to repeat. POSTs are safe only with an
# idempotency key: the engine then returns the first result instead of doing
# the work twice.
_SAFE = {"GET", "HEAD", "PUT", "DELETE"}


def _archive_name(name: str) -> str:
    """An entry's name, cleaned; raises ValueError for one that leaves the folder."""
    n = posixpath.normpath(name.replace("\\", "/").rstrip("/") or ".")
    if n.startswith("/") or n == ".." or n.startswith("../"):
        raise ValueError(f"unsafe path in archive: {name!r}")
    return n


def _inside(base: str, path: str) -> str:
    """path with every link resolved, which must stay inside base (links
    within the folder are followed, as os.Root follows them)."""
    real = os.path.realpath(path)
    if real != base and os.path.commonpath([base, real]) != base:
        raise ValueError(f"unsafe path in archive: {path!r} leads outside {base!r}")
    return real


def _replaceable(path: str) -> None:
    """Clears the way for a new entry: a file or link goes; a folder stays and
    the entry is refused (it would drop the folder's contents)."""
    try:
        st = os.lstat(path)
    except FileNotFoundError:
        return
    if stat.S_ISDIR(st.st_mode):
        raise ValueError(f"{path!r} is a folder")
    os.unlink(path)


def _extract(tf: "tarfile.TarFile", dest: str) -> int:
    """Merges a tar archive into dest with the rules of the Go client's
    save.ExtractTo, so every client pulls a folder the same way."""
    base = os.path.realpath(dest)
    dirs = []  # (path, mode, mtime): applied last, deepest first
    n = 0
    for m in tf:
        name = _archive_name(m.name)
        if name == ".":
            continue  # the folder itself
        mode = m.mode & 0o777
        parent = _inside(base, os.path.join(base, posixpath.dirname(name)))
        os.makedirs(parent, exist_ok=True)
        parent = _inside(base, parent)
        path = os.path.join(parent, posixpath.basename(name))
        if m.isdir():
            try:
                if not stat.S_ISDIR(os.lstat(path).st_mode):
                    os.unlink(path)
            except FileNotFoundError:
                pass
            try:
                os.mkdir(path, 0o700)
            except FileExistsError:
                pass
            dirs.append((path, mode, m.mtime))
        elif m.isreg():
            _replaceable(path)
            src = tf.extractfile(m)
            fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY | getattr(os, "O_NOFOLLOW", 0), 0o600)
            with os.fdopen(fd, "wb") as f:
                shutil.copyfileobj(src, f)
            os.chmod(path, mode)
            os.utime(path, (m.mtime, m.mtime))
        elif m.issym():
            _replaceable(path)
            os.symlink(m.linkname, path)  # stored as is; never followed out of dest
        elif m.islnk():
            target = _archive_name(m.linkname)
            if target == ".":
                raise ValueError(f"unsafe link target in archive: {m.linkname!r}")
            tparent = _inside(base, os.path.join(base, posixpath.dirname(target)))
            _replaceable(path)
            os.link(os.path.join(tparent, posixpath.basename(target)), path, follow_symlinks=False)
        else:
            continue  # devices, fifos and the like are not project files
        n += 1
    for path, mode, mtime in reversed(dirs):
        os.chmod(path, mode)
        os.utime(path, (mtime, mtime))
    return n


def config_path() -> str:
    """Where `cairn up` writes how to reach the local engine (the same file the CLI reads)."""
    if os.environ.get("CAIRN_CONFIG"):
        return os.environ["CAIRN_CONFIG"]
    base = os.environ.get("XDG_CONFIG_HOME") or os.path.join(os.path.expanduser("~"), ".config")
    return os.path.join(base, "cairn", "config.json")


def _read_config() -> dict:
    try:
        with open(config_path(), encoding="utf-8") as f:
            cfg = json.load(f)
        return cfg if isinstance(cfg, dict) else {}
    except (OSError, ValueError):
        return {}


class Client:
    """A client for one Cairn engine.

        client = cairn.Client("http://127.0.0.1:7420", token=os.environ.get("CAIRN_TOKEN"))
        ws = client.create_workspace(name="demo")
        print(ws.run("echo", "hello"))
        save = ws.save()
        save.resume()

    Requests that are safe to repeat are retried on a dropped connection or a
    busy engine (429), with backoff. create_workspace, save and copy send an
    idempotency key (one per call, reused by its retries), so a retry after a
    lost reply returns the original workspace or save instead of a second one.
    """

    def __init__(self, endpoint: Optional[str] = None, token: Optional[str] = None, *,
                 timeout: float = 60.0, retries: int = 4, ca_cert: Optional[str] = None):
        env_token = os.environ.get("CAIRN_TOKEN", "")
        if ca_cert is None:
            ca_cert = os.environ.get("CAIRN_CA_CERT", "")
        if not endpoint:
            endpoint = os.environ.get("CAIRN_ENDPOINT", "")
        if not endpoint:
            # the config `cairn up` writes; its token goes only to its endpoint
            cfg = _read_config()
            file_endpoint = cfg.get("endpoint") if isinstance(cfg.get("endpoint"), str) else ""
            endpoint = file_endpoint or "http://127.0.0.1:7420"
            if not ca_cert and file_endpoint and isinstance(cfg.get("ca_cert"), str):
                ca_cert = cfg["ca_cert"]
            if token is None and not env_token and file_endpoint:
                file_token = cfg.get("token", "")
                token = file_token if isinstance(file_token, str) else ""
        self.endpoint = endpoint.rstrip("/")
        self.token = token if token is not None else env_token
        self.timeout = timeout
        self.retries = retries
        # an https engine with a private CA: trust it besides the system's
        self._ssl = None
        if ca_cert:
            self._ssl = ssl.create_default_context()
            self._ssl.load_verify_locations(cafile=ca_cert)

    def __repr__(self) -> str:
        return f"cairn.Client({self.endpoint!r})"

    # -- engine ------------------------------------------------------------

    def health(self) -> None:
        """Raises unless the engine is up."""
        self._request("GET", "/healthz")

    def info(self) -> Info:
        """The engine's runtime and what it can guarantee (capabilities)."""
        return Info._from(self._json("GET", "/v1/info"))

    def metrics(self) -> str:
        """The engine's Prometheus metrics, as text."""
        return self._request("GET", "/metrics").decode()

    # -- workspaces --------------------------------------------------------

    def create_workspace(self, name: str = "", *, template: str = "", network: bool = False,
                         egress: Optional[list[str]] = None, secrets: Optional[list[str]] = None,
                         cpus: float = 0, memory_mb: int = 0, disk_mb: int = 0,
                         idempotency_key: Optional[str] = None) -> Workspace:
        """A new workspace, started from template (the engine's default when
        empty). Network is off unless asked for; egress limits it to these
        names ("pypi.org", "*.github.com") and IPv4 ranges, and switches it on."""
        body: dict[str, Any] = {"name": name, "network": network, "template": template,
                                "limits": Limits(cpus, memory_mb, disk_mb)._json()}
        if egress:
            body["egress"] = list(egress)
        if secrets:
            body["secrets"] = list(secrets)
        d = self._json("POST", "/v1/workspaces", body, key=idempotency_key or _key())
        return Workspace._from(d, self)

    # -- secrets -----------------------------------------------------------

    def set_secret(self, name: str, value: str, hosts: list[str]) -> dict:
        """Creates or replaces a secret: its value and the hosts it may be sent
        to. Workspaces created with secrets=[name] hold a placeholder; the
        engine never returns the value."""
        return self._json("PUT", "/v1/secrets/" + urllib.parse.quote(name, safe=""),
                          {"value": value, "hosts": list(hosts)})

    def secrets(self) -> list[dict]:
        """Secrets by name and hosts, never values."""
        return self._json("GET", "/v1/secrets").get("secrets") or []

    def delete_secret(self, name: str) -> None:
        self._request("DELETE", "/v1/secrets/" + urllib.parse.quote(name, safe=""))

    def workspaces(self) -> list[Workspace]:
        d = self._json("GET", "/v1/workspaces")
        return [Workspace._from(w, self) for w in d.get("workspaces") or []]

    def workspace(self, workspace_id: str) -> Workspace:
        return Workspace._from(self._json("GET", f"/v1/workspaces/{_q(workspace_id)}"), self)

    def pause(self, workspace_id: str) -> Workspace:
        """Frees the workspace's memory now; the next command or write wakes it
        with its processes. Pausing a paused or stopped workspace does nothing."""
        d = self._json("POST", f"/v1/workspaces/{_q(workspace_id)}/pause", safe=True)
        return Workspace._from(d, self)

    def delete_workspace(self, workspace_id: str) -> None:
        """Deletes the workspace (its saves are kept) and stops its commands."""
        self._request("DELETE", f"/v1/workspaces/{_q(workspace_id)}")

    # -- commands ----------------------------------------------------------

    def exec(self, workspace_id: str, command: Union[str, Sequence[str]], *,
             env: Optional[dict[str, str]] = None, cwd: str = "", stdin: Union[str, bytes] = "",
             timeout: Optional[float] = None, on_output: Optional[OutputCallback] = None) -> ExecResult:
        """Runs a command and returns how it ended.

        command is an argv list, or a string run with sh -c. timeout (seconds)
        stops the command and marks it timed out. With on_output, output is
        streamed: on_output(stream, data) is called with "stdout" or "stderr"
        and the exact bytes, in order, as the command writes them.

        Commands are never retried: running one twice is not safe in general.
        """
        argv = ["sh", "-c", command] if isinstance(command, str) else list(command)
        body: dict[str, Any] = {"command": argv}
        if env:
            body["env"] = env
        if cwd:
            body["dir"] = cwd
        if stdin:
            # The API takes stdin as text; bytes must be valid UTF-8.
            body["stdin"] = stdin.decode() if isinstance(stdin, bytes) else stdin
        if timeout:
            body["timeout_ms"] = int(timeout * 1000)
        path = f"/v1/workspaces/{_q(workspace_id)}/exec"
        # The HTTP request lives as long as the command: no socket timeout
        # unless the command has one (plus room for the engine to answer).
        http_timeout = timeout + 30 if timeout else None
        if on_output is None:
            d = json.loads(self._request("POST", path, _body(body), retry=False, timeout=http_timeout))
            return ExecResult(exit_code=d.get("exit_code", 0), timed_out=d.get("timed_out", False),
                              stdout=_output(d, "stdout"), stderr=_output(d, "stderr"),
                              duration_ms=d.get("duration_ms", 0),
                              stdout_truncated=d.get("stdout_truncated", False),
                              stderr_truncated=d.get("stderr_truncated", False))
        body["stream"] = True
        with self._open("POST", path, _body(body), timeout=http_timeout) as resp:
            for line in resp:
                if not line.strip():
                    continue
                ev = json.loads(line)
                kind = ev.get("type")
                if kind in ("stdout", "stderr"):
                    on_output(kind, base64.b64decode(ev.get("data") or ""))
                elif kind == "exit":
                    return ExecResult(exit_code=ev.get("exit_code") or 0, timed_out=ev.get("timed_out", False),
                                      stdout=b"", stderr=b"", duration_ms=ev.get("duration_ms", 0))
                elif kind == "error":
                    e = ev.get("error") or {}
                    raise from_response(resp.status, e.get("code", "internal"), e.get("message", ""),
                                        resp.headers.get("X-Request-Id", ""))
        raise ConnectionFailed("the output stream ended before the command did")

    def run(self, workspace_id: str, *argv: str, **kw) -> str:
        """Runs argv (or one shell string) and returns stdout as text; raises
        CommandFailed on a non-zero exit or a timeout."""
        command: Union[str, Sequence[str]] = argv[0] if len(argv) == 1 else list(argv)
        res = self.exec(workspace_id, command, **kw)
        if not res.ok:
            shown = command if isinstance(command, str) else " ".join(command)
            why = "timed out" if res.timed_out else f"exited {res.exit_code}"
            raise CommandFailed(f"{shown!r} {why}: {res.stderr.decode('utf-8', 'replace').strip()}", res)
        return res.text

    # -- sessions ----------------------------------------------------------

    def create_session(self, workspace_id: str, language: str = "python") -> Session:
        """Starts a "python" or "shell" session in a workspace. Not retried
        after a dropped connection (a second session might start): list
        sessions to find it."""
        d = self._json("POST", f"/v1/workspaces/{_q(workspace_id)}/sessions", {"language": language})
        return Session._from(d, workspace_id, self)

    def sessions(self, workspace_id: str) -> list[Session]:
        d = self._json("GET", f"/v1/workspaces/{_q(workspace_id)}/sessions")
        return [Session._from(s, workspace_id, self) for s in d.get("sessions") or []]

    def run_cell(self, workspace_id: str, session_id: str, code: str, *,
                 timeout: Optional[float] = None) -> RunResult:
        """Runs code in a session and returns how it ended. One cell runs at a
        time per session (Conflict otherwise). Never retried: running a cell
        twice is not safe in general."""
        body: dict[str, Any] = {"code": code}
        if timeout:
            body["timeout_ms"] = int(timeout * 1000)
        path = f"/v1/workspaces/{_q(workspace_id)}/sessions/{_q(session_id)}/run"
        http_timeout = timeout + 60 if timeout else None
        d = json.loads(self._request("POST", path, _body(body), retry=False, timeout=http_timeout))
        return RunResult(exit_code=d.get("exit_code", 0), timed_out=d.get("timed_out", False),
                         stdout=_output(d, "stdout"), stderr=_output(d, "stderr"),
                         duration_ms=d.get("duration_ms", 0), stdout_truncated=d.get("stdout_truncated", False),
                         stderr_truncated=d.get("stderr_truncated", False), ended=d.get("ended", False))

    def delete_session(self, workspace_id: str, session_id: str) -> None:
        """Stops a session and everything it started."""
        self._request("DELETE", f"/v1/workspaces/{_q(workspace_id)}/sessions/{_q(session_id)}")

    # -- files -------------------------------------------------------------

    def write_file(self, workspace_id: str, path: str, content: Content) -> None:
        """Writes a file (bytes, text, an open binary file or a local path),
        creating folders as needed. Large files are streamed."""
        data, close = _content(content)
        try:
            self._request("PUT", self._file_path(workspace_id, path), data)
        finally:
            if close:
                data.close()

    def read_file(self, workspace_id: str, path: str) -> bytes:
        return self._request("GET", self._file_path(workspace_id, path))

    def download_file(self, workspace_id: str, path: str, dest: Union[str, "os.PathLike[str]", IO[bytes]]) -> int:
        """Streams a file out of the workspace into dest (a local path or an
        open binary file) without holding it in memory. Returns its size."""
        out, close = (open(dest, "wb"), True) if isinstance(dest, (str, os.PathLike)) else (dest, False)
        n = 0
        try:
            with self._open("GET", self._file_path(workspace_id, path)) as resp:
                while chunk := resp.read(1 << 20):
                    out.write(chunk)
                    n += len(chunk)
        finally:
            if close:
                out.close()
        return n

    def push_dir(self, workspace_id: str, local_dir: Union[str, "os.PathLike[str]"], remote_dir: str,
                 exclude: Sequence[str] = ()) -> dict:
        """Copies a local folder into remote_dir of the workspace (created if
        missing), merging with what is there: modes, times, symlinks and empty
        folders are kept, ownership is not. exclude holds fnmatch patterns
        matched against each entry's name and its path relative to local_dir
        (e.g. "node_modules", "*.pyc"); a matching folder is left out whole.
        Returns what was copied: files, dirs, links and bytes."""
        root = os.fspath(local_dir)

        def keep(info: tarfile.TarInfo) -> Optional[tarfile.TarInfo]:
            rel = info.name[2:] if info.name.startswith("./") else info.name
            if rel not in (".", "") and any(fnmatch.fnmatchcase(os.path.basename(rel), p) or fnmatch.fnmatchcase(rel, p)
                                            for p in exclude):
                return None
            if not (info.isreg() or info.isdir() or info.issym()):
                return None  # devices, fifos and the like are not project files
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            return info

        # spooled to a temporary file so a retry after a dropped connection can
        # send it again (a push merges, so repeating it is safe)
        with tempfile.TemporaryFile() as buf:
            with tarfile.open(fileobj=buf, mode="w", format=tarfile.PAX_FORMAT) as tf:
                for name in sorted(os.listdir(root)):
                    tf.add(os.path.join(root, name), arcname=name, filter=keep)
            buf.seek(0)
            q = urllib.parse.urlencode({"path": remote_dir})
            return json.loads(self._request("PUT", f"/v1/workspaces/{_q(workspace_id)}/archive?{q}", buf))

    def pull_dir(self, workspace_id: str, remote_dir: str, local_dir: Union[str, "os.PathLike[str]"]) -> int:
        """Copies remote_dir of the workspace into a local folder (created if
        missing), merging with what is there, as the Go client and `cairn pull`
        do: folders are kept, a file or link replaces a file or link of the
        same name (never writing through a link), permission bits and times
        are kept (not setuid/setgid/sticky, not ownership). Nothing extracted
        can land outside local_dir. Returns the number of entries copied."""
        dest = os.fspath(local_dir)
        os.makedirs(dest, exist_ok=True)
        q = urllib.parse.urlencode({"path": remote_dir})
        with tempfile.TemporaryFile() as buf:
            with self._open("GET", f"/v1/workspaces/{_q(workspace_id)}/archive?{q}") as resp:
                while chunk := resp.read(1 << 20):
                    buf.write(chunk)
            buf.seek(0)
            with tarfile.open(fileobj=buf, mode="r:") as tf:
                return _extract(tf, dest)

    def _file_path(self, workspace_id: str, path: str) -> str:
        return f"/v1/workspaces/{_q(workspace_id)}/files?" + urllib.parse.urlencode({"path": path})

    # -- saves -------------------------------------------------------------

    def save(self, workspace_id: str, kind: str = "quick", *, keep_running: bool = False,
             idempotency_key: Optional[str] = None) -> Save:
        """Saves the workspace. "quick" keeps files; "full" keeps memory and
        running processes too, and stops the workspace until it is resumed,
        unless keep_running=True: then it carries on straight away, its
        processes still running (save before a risky step, keep working)."""
        body: dict[str, Any] = {"kind": kind}
        if keep_running:
            body["keep_running"] = True
        d = self._json("POST", f"/v1/workspaces/{_q(workspace_id)}/saves", body,
                       key=idempotency_key or _key())
        return Save._from(d, self)

    def saves(self, workspace_id: str) -> list[Save]:
        d = self._json("GET", f"/v1/workspaces/{_q(workspace_id)}/saves")
        return [Save._from(s, self) for s in d.get("saves") or []]

    # --- activity log ---

    def events(self, workspace_id: str, *, after: int = 0, limit: int = 0, last: int = 0,
               kind: str = "") -> list[Event]:
        """A workspace's activity log, oldest first: commands with their exit
        code and output tail, files written and read, saves, resumes, copies,
        pauses, session cells. after= pages past an event ID, last= gives the
        newest events, kind= filters ("exec"; "session_*" for a prefix)."""
        d = self._json("GET", self._events_path(workspace_id, after, limit, last, kind))
        return [Event._from(e) for e in d.get("events") or []]

    def follow_events(self, workspace_id: str, *, after: int = 0, last: int = 0,
                      kind: str = "") -> Iterator[Event]:
        """Yields the events selected (as events() would), then each new one as
        it happens, until the caller stops iterating or the engine shuts down
        (ConnectionFailed)."""
        path = self._events_path(workspace_id, after, 0, last, kind, follow=True)
        with self._open("GET", path, timeout=None) as resp:
            for line in resp:
                if line.strip():
                    yield Event._from(json.loads(line))
        raise ConnectionFailed("the engine ended the event stream")

    def _events_path(self, workspace_id: str, after: int, limit: int, last: int, kind: str,
                     follow: bool = False) -> str:
        q: dict[str, Any] = {}
        if after:
            q["after"] = after
        if last:
            q["last"] = last
        elif limit:
            q["limit"] = limit
        if kind:
            q["kind"] = kind
        if follow:
            q["follow"] = 1
        path = f"/v1/workspaces/{_q(workspace_id)}/events"
        return path + ("?" + urllib.parse.urlencode(q) if q else "")

    def get_save(self, save_id: str) -> Save:
        return Save._from(self._json("GET", f"/v1/saves/{_q(save_id)}"), self)

    def resume(self, save_id: str) -> Workspace:
        """Puts the save's own workspace back to exactly the saved state."""
        return Workspace._from(self._json("POST", f"/v1/saves/{_q(save_id)}/resume"), self)

    def copy(self, save_id: str, count: int = 1, *, idempotency_key: Optional[str] = None) -> list[Workspace]:
        """New, independent workspaces that each start from the save."""
        d = self._json("POST", f"/v1/saves/{_q(save_id)}/copies", {"count": count}, key=idempotency_key or _key())
        return [Workspace._from(w, self) for w in d.get("workspaces") or []]

    def delete_save(self, save_id: str) -> None:
        """Hides the save; undelete_save brings it back until its purge time."""
        self._request("DELETE", f"/v1/saves/{_q(save_id)}")

    def undelete_save(self, save_id: str) -> Save:
        return Save._from(self._json("POST", f"/v1/saves/{_q(save_id)}/undelete", safe=True), self)

    def diff_saves(self, save_id: str, from_save: Optional[str] = None, limit: int = 0) -> SaveDiff:
        """Files added, modified and removed from from_save (default: the save
        save_id came from) to save_id, read from the saves without restoring.
        limit caps the changes listed (0: the engine's default, 1000)."""
        q = {}
        if from_save:
            q["from"] = from_save
        if limit > 0:
            q["limit"] = str(limit)
        path = f"/v1/saves/{_q(save_id)}/diff" + ("?" + urllib.parse.urlencode(q) if q else "")
        return SaveDiff._from(self._json("GET", path))

    def pin_save(self, save_id: str) -> Save:
        """Keeps a save from the engine's pruning; delete refuses it until unpinned."""
        return Save._from(self._json("POST", f"/v1/saves/{_q(save_id)}/pin", safe=True), self)

    def unpin_save(self, save_id: str) -> Save:
        return Save._from(self._json("DELETE", f"/v1/saves/{_q(save_id)}/pin", safe=True), self)

    def verify_save(self, save_id: str) -> Save:
        """Re-checks the save's checksums: state "ready", or "corrupt"/"missing"."""
        return Save._from(self._json("POST", f"/v1/saves/{_q(save_id)}/verify", safe=True), self)

    # -- templates ---------------------------------------------------------

    def templates(self) -> list[Template]:
        d = self._json("GET", "/v1/templates")
        return [Template._from(t) for t in d.get("templates") or []]

    def template(self, name: str) -> Template:
        return Template._from(self._json("GET", f"/v1/templates/{_q(name)}"))

    def import_template(self, name: str, rootfs_tar: Content) -> Template:
        """Imports a root filesystem tar as a template, for example the output of
        `docker export $(docker create python:3.12-slim)`."""
        data, close = _content(rootfs_tar)
        try:
            return Template._from(json.loads(self._request("PUT", f"/v1/templates/{_q(name)}", data)))
        finally:
            if close:
                data.close()

    def pull_template(self, name: str, image: str, *, username: str = "", password: str = "",
                      insecure: bool = False) -> Template:
        """Builds a template from a container image in a registry (the image for
        the engine's architecture, all layers applied). The login is used for
        this pull only; the engine never stores it. insecure talks plain HTTP to
        the registry."""
        body: dict[str, Any] = {"image": image, "insecure": insecure}
        if username:
            body["username"], body["password"] = username, password
        return Template._from(self._json("POST", f"/v1/templates/{_q(name)}/pull", body))

    def delete_template(self, name: str) -> None:
        self._request("DELETE", f"/v1/templates/{_q(name)}")

    # -- transport ---------------------------------------------------------

    def _json(self, method: str, path: str, body: Any = None, *, key: Optional[str] = None, safe: bool = False) -> Any:
        raw = self._request(method, path, _body(body) if body is not None else None, key=key, safe=safe)
        return json.loads(raw) if raw else {}

    def _request(self, method: str, path: str, data: Any = None, *, key: Optional[str] = None,
                 safe: bool = False, retry: bool = True, timeout: Any = "default") -> bytes:
        with self._open(method, path, data, key=key, safe=safe, retry=retry, timeout=timeout) as resp:
            return resp.read()

    def _open(self, method: str, path: str, data: Any = None, *, key: Optional[str] = None,
              safe: bool = False, retry: bool = True, timeout: Any = "default"):
        """Sends a request and returns the open response, retrying what is safe
        to retry. A busy engine (429) did no work, so that is always retried;
        retry=False only stops retries after a dropped connection, where the
        request may have been carried out."""
        repeatable = retry and (safe or key is not None or method in _SAFE)
        start = data.tell() if hasattr(data, "seek") else None
        headers = {"User-Agent": f"cairn-python/{__version__}"}
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"
        if key:
            headers["Idempotency-Key"] = key
        if data is not None:
            headers["Content-Type"] = "application/json" if isinstance(data, _JSONBody) else "application/octet-stream"
        if timeout == "default":
            timeout = self.timeout
        attempt = 0
        while True:
            attempt += 1
            if start is not None:
                data.seek(start)
            payload = bytes(data) if isinstance(data, _JSONBody) else data
            req = urllib.request.Request(self.endpoint + path, data=payload, method=method, headers=headers)
            try:
                return urllib.request.urlopen(req, timeout=timeout, context=self._ssl)
            except urllib.error.HTTPError as e:
                err = _http_error(e)
                if isinstance(err, TooManyRequests) and attempt <= self.retries:
                    _sleep(attempt, e.headers.get("Retry-After"))
                    continue
                raise err from None
            except (urllib.error.URLError, ConnectionError, socket.timeout, TimeoutError) as e:
                rewindable = data is None or isinstance(data, (bytes, _JSONBody)) or start is not None
                if repeatable and rewindable and attempt <= self.retries:
                    _sleep(attempt, None)
                    continue
                reason = getattr(e, "reason", e)
                raise ConnectionFailed(f"{method} {path}: {reason}") from None


class _JSONBody(bytes):
    """A request body that is JSON (as opposed to file content)."""


def _body(obj: Any) -> _JSONBody:
    return _JSONBody(json.dumps(obj).encode())


def _content(content: Content):
    """Returns (body, close?) for file content in any of the accepted forms."""
    if isinstance(content, str):
        return content.encode(), False
    if isinstance(content, (bytes, bytearray)):
        return bytes(content), False
    if isinstance(content, os.PathLike):
        return open(content, "rb"), True
    if hasattr(content, "read"):
        return content, False
    raise TypeError(f"file content must be bytes, str, a binary file or a path, not {type(content).__name__}")


def _output(d: dict, name: str) -> bytes:
    v = d.get(name) or ""
    if d.get(f"{name}_encoding") == "base64":
        return base64.b64decode(v)
    return v.encode()


def _http_error(e: urllib.error.HTTPError) -> CairnError:
    try:
        raw = e.read()
    finally:
        e.close()
    rid = e.headers.get("X-Request-Id", "")
    try:
        err = json.loads(raw)["error"]
        return from_response(e.code, err["code"], err.get("message", ""), rid)
    except (ValueError, KeyError, TypeError):
        return CairnError(raw.decode("utf-8", "replace").strip() or e.reason, status=e.code, code="http_error", request_id=rid)


def _sleep(attempt: int, retry_after: Optional[str]) -> None:
    try:
        wait = float(retry_after) if retry_after else 0.0
    except ValueError:
        wait = 0.0
    backoff = min(0.25 * 2 ** (attempt - 1), 4.0)
    time.sleep(max(wait, backoff) * (0.8 + 0.4 * random.random()))


def _key() -> str:
    return uuid.uuid4().hex


def _q(s: str) -> str:
    return urllib.parse.quote(s, safe="")
