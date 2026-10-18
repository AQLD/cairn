"""Errors raised by the Cairn client.

Every error the engine returns carries one of its stable codes (see the API's
error codes); each code has its own subclass so callers can catch exactly what
they handle:

    try:
        client.resume(save_id)
    except cairn.SaveCorrupt:
        ...
"""

from __future__ import annotations


class CairnError(Exception):
    """An error returned by the engine (or raised by the client for it)."""

    code = "internal"

    def __init__(self, message: str, *, status: int = 0, code: str | None = None, request_id: str = ""):
        super().__init__(message)
        self.message = message
        self.status = status
        if code is not None:
            self.code = code
        # Matches the engine's log line for this request.
        self.request_id = request_id

    def __str__(self) -> str:
        s = f"cairn: {self.code}"
        if self.status:
            s += f" ({self.status})"
        s += f": {self.message}"
        if self.request_id:
            s += f" [request {self.request_id}]"
        return s


class NotFound(CairnError):
    code = "not_found"


class InvalidArgument(CairnError):
    code = "invalid_argument"


class Conflict(CairnError):
    """The workspace or save is in a state that doesn't allow this right now
    (for example busy with a long command, or stopped)."""

    code = "conflict"


class SaveCorrupt(CairnError):
    code = "save_corrupt"


class InsufficientStorage(CairnError):
    code = "insufficient_storage"


class TooManyRequests(CairnError):
    code = "too_many_requests"


class UnsupportedByRuntime(CairnError):
    """The engine's runtime can't do this (for example a full save on the
    host runtime). Never a silent partial success."""

    code = "unsupported_by_runtime"


class Unauthorized(CairnError):
    code = "unauthorized"


class InternalError(CairnError):
    code = "internal"


class ConnectionFailed(CairnError):
    """The engine could not be reached, or the connection dropped before an
    answer. The request may or may not have been carried out: retry it with
    the same idempotency key (the client does this for you)."""

    code = "connection_failed"


class CommandFailed(CairnError):
    """Raised by run() when a command exits non-zero or times out."""

    code = "command_failed"

    def __init__(self, message: str, result):
        super().__init__(message)
        self.result = result


_BY_CODE = {
    c.code: c
    for c in (NotFound, InvalidArgument, Conflict, SaveCorrupt, InsufficientStorage,
              TooManyRequests, UnsupportedByRuntime, Unauthorized, InternalError)
}


def from_response(status: int, code: str, message: str, request_id: str) -> CairnError:
    cls = _BY_CODE.get(code, CairnError)
    return cls(message, status=status, code=code, request_id=request_id)
