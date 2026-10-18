"""Python client for Cairn: private Linux workspaces for AI agents that can be
saved, resumed and copied.

    import cairn

    client = cairn.Client("http://127.0.0.1:7420")
    ws = client.create_workspace(name="demo")
    ws.run("sh", "-c", "echo hello > note.txt")
    save = ws.save()
    ws.run("sh", "-c", "echo changed > note.txt")
    save.resume()
    assert ws.read_file("note.txt") == b"hello\\n"
"""

__version__ = "0.1.0"

from ._client import Client  # noqa: E402
from ._errors import (  # noqa: E402
    CairnError,
    CommandFailed,
    Conflict,
    ConnectionFailed,
    InsufficientStorage,
    InternalError,
    InvalidArgument,
    NotFound,
    SaveCorrupt,
    TooManyRequests,
    Unauthorized,
    UnsupportedByRuntime,
)
from ._types import Capabilities, ExecResult, Info, Limits, Save, Template, Workspace  # noqa: E402

__all__ = [
    "Client", "Workspace", "Save", "Template", "Info", "Capabilities", "Limits", "ExecResult",
    "CairnError", "NotFound", "InvalidArgument", "Conflict", "SaveCorrupt", "InsufficientStorage",
    "TooManyRequests", "UnsupportedByRuntime", "Unauthorized", "InternalError", "ConnectionFailed",
    "CommandFailed",
]
