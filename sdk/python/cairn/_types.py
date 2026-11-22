"""What the engine returns, as plain dataclasses.

Workspace and Save also carry the client that fetched them, so they can act on
themselves: ws.run("ls"), ws.save(), save.resume().
"""

from __future__ import annotations

import dataclasses
from dataclasses import dataclass, field
from datetime import datetime
from typing import TYPE_CHECKING, Any, Callable, Iterator, Optional

if TYPE_CHECKING:
    from ._client import Client


def _time(v: Optional[str]) -> Optional[datetime]:
    if not v:
        return None
    # Go writes RFC 3339 with nanoseconds; Python's parser takes microseconds.
    if v.endswith("Z"):
        v = v[:-1] + "+00:00"
    date, _, rest = v.partition("T")
    if "." in rest:
        clock, _, frac = rest.partition(".")
        digits = "".join(ch for ch in frac if ch.isdigit())
        zone = frac[len(digits):]
        rest = f"{clock}.{digits[:6].ljust(6, '0')}{zone}"
    return datetime.fromisoformat(f"{date}T{rest}")


def _pick(cls, data: dict) -> dict:
    names = {f.name for f in dataclasses.fields(cls)}
    return {k: v for k, v in data.items() if k in names}


@dataclass
class Capabilities:
    isolation: bool = False
    full_save: bool = False
    resource_limits: bool = False
    disk_limits: bool = False
    network_control: bool = False
    persistent_processes: bool = False
    shared_templates: bool = False  # workspaces share their template's files read-only


@dataclass
class Info:
    runtime: str
    runtime_version: str
    capabilities: Capabilities
    save_format: int

    @classmethod
    def _from(cls, d: dict) -> "Info":
        return cls(runtime=d.get("runtime", ""), runtime_version=d.get("runtime_version", ""),
                   capabilities=Capabilities(**_pick(Capabilities, d.get("capabilities") or {})),
                   save_format=d.get("save_format", 0))


@dataclass
class Limits:
    cpus: float = 0
    memory_mb: int = 0
    disk_mb: int = 0

    def _json(self) -> dict:
        return {k: v for k, v in dataclasses.asdict(self).items() if v}


@dataclass
class Template:
    name: str
    size: int
    sha256: str
    created_at: Optional[datetime]
    image: str = ""  # the container image it was pulled from, if any
    image_digest: str = ""

    @classmethod
    def _from(cls, d: dict) -> "Template":
        return cls(name=d["name"], size=d.get("size", 0), sha256=d.get("sha256", ""), created_at=_time(d.get("created_at")),
                   image=d.get("image", ""), image_digest=d.get("image_digest", ""))


@dataclass
class ExecResult:
    """How a command ended. stdout and stderr are the exact bytes it wrote (up
    to 4 MiB each; the *_truncated flags say if more was cut off). When the
    command was streamed, they are empty: the output went to on_output."""

    exit_code: int
    timed_out: bool
    stdout: bytes
    stderr: bytes
    duration_ms: int
    stdout_truncated: bool = False
    stderr_truncated: bool = False

    @property
    def ok(self) -> bool:
        return self.exit_code == 0 and not self.timed_out

    @property
    def text(self) -> str:
        """stdout decoded as UTF-8 (invalid bytes replaced)."""
        return self.stdout.decode("utf-8", "replace")


@dataclass
class RunResult(ExecResult):
    """How a cell ended in a session. In Python sessions a final expression's
    value is printed, as in a notebook; an exception's traceback goes to
    stderr with exit_code 1. timed_out: the cell was interrupted at its
    timeout and the session carries on. ended: the session's process died
    during the cell (it exited, or was stopped for memory)."""

    ended: bool = False

    @property
    def ok(self) -> bool:
        return self.exit_code == 0 and not self.timed_out and not self.ended


@dataclass
class Session:
    """An interpreter kept running in a workspace: variables, imports, loaded
    data and the working folder carry over between runs. Full saves and
    pauses keep it, like any other process; a quick save does not."""

    id: str
    workspace_id: str
    language: str  # python or shell
    pid: int
    cells: int
    _client: Any = field(default=None, repr=False, compare=False)

    @classmethod
    def _from(cls, d: dict, workspace_id: str, client: "Client") -> "Session":
        return cls(id=d["id"], workspace_id=workspace_id, language=d.get("language", ""), pid=d.get("pid", 0),
                   cells=d.get("cells", 0), _client=client)

    def run(self, code: str, *, timeout: Optional[float] = None) -> RunResult:
        return self._client.run_cell(self.workspace_id, self.id, code, timeout=timeout)

    def delete(self) -> None:
        self._client.delete_session(self.workspace_id, self.id)


@dataclass
class Workspace:
    id: str
    name: str
    state: str  # running, stopped (full-saved), paused (woken on next use), ...
    limits: Limits
    network: bool
    template: str
    source_save: str
    created_at: Optional[datetime]
    updated_at: Optional[datetime]
    base_save: str = ""  # the save its state comes from: last saved, resumed or copied
    _client: Any = field(default=None, repr=False, compare=False)

    @classmethod
    def _from(cls, d: dict, client: "Client") -> "Workspace":
        return cls(id=d["id"], name=d.get("name", ""), state=d.get("state", ""),
                   limits=Limits(**_pick(Limits, d.get("limits") or {})), network=d.get("network", False),
                   template=d.get("template", ""), source_save=d.get("source_save", ""),
                   created_at=_time(d.get("created_at")), updated_at=_time(d.get("updated_at")),
                   base_save=d.get("base_save", ""), _client=client)

    def exec(self, command, **kw) -> ExecResult:
        return self._client.exec(self.id, command, **kw)

    def run(self, *argv: str, **kw) -> str:
        return self._client.run(self.id, *argv, **kw)

    def write_file(self, path: str, content) -> None:
        self._client.write_file(self.id, path, content)

    def read_file(self, path: str) -> bytes:
        return self._client.read_file(self.id, path)

    def push(self, local_dir, remote_dir: str, exclude=()) -> dict:
        """Copies a local folder into the workspace, merging (see Client.push_dir)."""
        return self._client.push_dir(self.id, local_dir, remote_dir, exclude)

    def pull(self, remote_dir: str, local_dir) -> int:
        """Copies a folder of the workspace to the local machine, merging."""
        return self._client.pull_dir(self.id, remote_dir, local_dir)

    def save(self, kind: str = "quick", **kw) -> "Save":
        return self._client.save(self.id, kind, **kw)

    def saves(self) -> list["Save"]:
        return self._client.saves(self.id)

    def pause(self) -> "Workspace":
        return self._client.pause(self.id)

    def session(self, language: str = "python") -> Session:
        return self._client.create_session(self.id, language)

    def sessions(self) -> list[Session]:
        return self._client.sessions(self.id)

    def events(self, **kw) -> list["Event"]:
        """The activity log: what was run, written, saved... in this workspace."""
        return self._client.events(self.id, **kw)

    def follow_events(self, **kw) -> Iterator["Event"]:
        """Yields the log's events, then each new one as it happens."""
        return self._client.follow_events(self.id, **kw)

    def refresh(self) -> "Workspace":
        return self._client.workspace(self.id)

    def delete(self) -> None:
        self._client.delete_workspace(self.id)


@dataclass
class Event:
    """One entry of a workspace's activity log."""
    id: int  # increasing: pass the last one seen as after=
    workspace_id: str
    time: Optional[datetime]
    kind: str  # exec, file_write, file_read, save, resume, copy, create, delete, pause, wake, session_*
    summary: str  # one readable line
    request_id: str = ""
    error: str = ""  # set when the operation failed
    data: dict = field(default_factory=dict)  # exit_code, stdout_tail, save, ... by kind

    @classmethod
    def _from(cls, d: dict) -> "Event":
        return cls(id=d.get("id", 0), workspace_id=d.get("workspace_id", ""), time=_time(d.get("time")),
                   kind=d.get("kind", ""), summary=d.get("summary", ""), request_id=d.get("request_id", ""),
                   error=d.get("error", ""), data=d.get("data") or {})


@dataclass
class Change:
    """One path that differs between two saves."""

    path: str  # absolute, as the workspace sees it
    change: str  # added, modified or removed
    type: str  # file, dir or symlink
    size: int = 0  # a file's size

    @classmethod
    def _from(cls, d: dict) -> "Change":
        return cls(path=d.get("path", ""), change=d.get("change", ""), type=d.get("type", ""), size=d.get("size", 0))


@dataclass
class SaveDiff:
    """What changed in the files from one save (from_save) to another (to_save).

    A path is modified when its content, type, link target or permissions
    differ, not its modification time alone. Memory and processes are not
    compared. ``truncated`` says ``changes`` stops at the limit; the counts
    include every change.
    """

    from_save: str
    to_save: str
    added: int
    modified: int
    removed: int
    changes: list
    truncated: bool = False

    @classmethod
    def _from(cls, d: dict) -> "SaveDiff":
        return cls(from_save=d.get("from", ""), to_save=d.get("to", ""), added=d.get("added", 0),
                   modified=d.get("modified", 0), removed=d.get("removed", 0),
                   changes=[Change._from(c) for c in d.get("changes") or []], truncated=d.get("truncated", False))


@dataclass
class Save:
    id: str
    workspace_id: str
    kind: str  # quick (files) or full (memory and processes too)
    state: str  # ready, missing, corrupt, deleted
    size: int
    created_at: Optional[datetime]
    deleted_at: Optional[datetime] = None
    purge_after: Optional[datetime] = None  # a deleted save can be undeleted until then
    auto: bool = False  # made by the engine to pause the workspace
    uploaded: bool = False  # a copy is in object storage
    parent: str = ""  # the save the workspace's state came from when this one was made
    pinned: bool = False  # kept by pruning; delete refuses it until unpinned
    _client: Any = field(default=None, repr=False, compare=False)

    @classmethod
    def _from(cls, d: dict, client: "Client") -> "Save":
        return cls(id=d["id"], workspace_id=d.get("workspace_id", ""), kind=d.get("kind", ""), state=d.get("state", ""),
                   size=d.get("size", 0), created_at=_time(d.get("created_at")), deleted_at=_time(d.get("deleted_at")),
                   purge_after=_time(d.get("purge_after")), auto=d.get("auto", False), uploaded=d.get("uploaded", False),
                   parent=d.get("parent", ""), pinned=d.get("pinned", False), _client=client)

    def resume(self) -> Workspace:
        return self._client.resume(self.id)

    def copy(self, count: int = 1, **kw) -> list[Workspace]:
        return self._client.copy(self.id, count, **kw)

    def pin(self) -> "Save":
        """Keeps this save from the engine's pruning (and from delete until unpinned)."""
        return self._client.pin_save(self.id)

    def unpin(self) -> "Save":
        return self._client.unpin_save(self.id)

    def diff(self, from_save: Optional[str] = None, limit: int = 0) -> SaveDiff:
        """Files changed since from_save, or since the save this one came from."""
        return self._client.diff_saves(self.id, from_save, limit)

    def delete(self) -> None:
        self._client.delete_save(self.id)

    def verify(self) -> "Save":
        return self._client.verify_save(self.id)


OutputCallback = Callable[[str, bytes], None]
