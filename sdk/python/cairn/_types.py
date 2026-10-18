"""What the engine returns, as plain dataclasses.

Workspace and Save also carry the client that fetched them, so they can act on
themselves: ws.run("ls"), ws.save(), save.resume().
"""

from __future__ import annotations

import dataclasses
from dataclasses import dataclass, field
from datetime import datetime
from typing import TYPE_CHECKING, Any, Callable, Optional

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
    _client: Any = field(default=None, repr=False, compare=False)

    @classmethod
    def _from(cls, d: dict, client: "Client") -> "Workspace":
        return cls(id=d["id"], name=d.get("name", ""), state=d.get("state", ""),
                   limits=Limits(**_pick(Limits, d.get("limits") or {})), network=d.get("network", False),
                   template=d.get("template", ""), source_save=d.get("source_save", ""),
                   created_at=_time(d.get("created_at")), updated_at=_time(d.get("updated_at")), _client=client)

    def exec(self, command, **kw) -> ExecResult:
        return self._client.exec(self.id, command, **kw)

    def run(self, *argv: str, **kw) -> str:
        return self._client.run(self.id, *argv, **kw)

    def write_file(self, path: str, content) -> None:
        self._client.write_file(self.id, path, content)

    def read_file(self, path: str) -> bytes:
        return self._client.read_file(self.id, path)

    def save(self, kind: str = "quick", **kw) -> "Save":
        return self._client.save(self.id, kind, **kw)

    def saves(self) -> list["Save"]:
        return self._client.saves(self.id)

    def pause(self) -> "Workspace":
        return self._client.pause(self.id)

    def refresh(self) -> "Workspace":
        return self._client.workspace(self.id)

    def delete(self) -> None:
        self._client.delete_workspace(self.id)


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
    _client: Any = field(default=None, repr=False, compare=False)

    @classmethod
    def _from(cls, d: dict, client: "Client") -> "Save":
        return cls(id=d["id"], workspace_id=d.get("workspace_id", ""), kind=d.get("kind", ""), state=d.get("state", ""),
                   size=d.get("size", 0), created_at=_time(d.get("created_at")), deleted_at=_time(d.get("deleted_at")),
                   purge_after=_time(d.get("purge_after")), auto=d.get("auto", False), uploaded=d.get("uploaded", False),
                   _client=client)

    def resume(self) -> Workspace:
        return self._client.resume(self.id)

    def copy(self, count: int = 1, **kw) -> list[Workspace]:
        return self._client.copy(self.id, count, **kw)

    def delete(self) -> None:
        self._client.delete_save(self.id)

    def verify(self) -> "Save":
        return self._client.verify_save(self.id)


OutputCallback = Callable[[str, bytes], None]
