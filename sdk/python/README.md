# Cairn Python SDK

A Python client for [Cairn](https://github.com/AQLD/cairn). Cairn gives each AI agent a private Linux workspace it can save, resume and copy. The client uses only the standard library; Python 3.9 or newer.

```sh
pip install ./sdk/python        # from a checkout of the repository
```

```python
import cairn

client = cairn.Client("http://127.0.0.1:7420", token="...")  # or CAIRN_ENDPOINT / CAIRN_TOKEN

ws = client.create_workspace(name="demo", memory_mb=1024)
ws.write_file("app.py", "print('hello')")
print(ws.run("python3", "app.py"))           # "hello\n"; raises CommandFailed on a non-zero exit

save = ws.save()                               # quick save: files
ws.run("rm app.py")
save.resume()                                  # app.py is back

copies = save.copy(3)                          # three independent workspaces from one save
```

## Commands

`exec` returns how a command ended, with the exact bytes it wrote:

```python
r = ws.exec(["pytest", "-q"], cwd="repo", env={"CI": "1"}, timeout=600)
r.exit_code, r.timed_out, r.stdout, r.stderr   # stdout and stderr are bytes
```

A string runs with `sh -c`. Pass `on_output` to stream output while the command runs:

```python
ws.exec("make build", on_output=lambda stream, data: print(stream, data.decode(), end=""))
```

## Sessions

A session is an interpreter kept running in the workspace, so variables, imports, loaded data and the working folder carry over between calls, as in a notebook:

```python
py = ws.session("python")              # or "shell"
py.run("import pandas as pd; df = pd.read_csv('sales.csv')")
print(py.run("df.amount.sum()").text)  # a final expression is printed
r = py.run("train()", timeout=600)     # past the timeout the cell is interrupted; the session carries on
r.ok, r.exit_code, r.timed_out, r.ended, r.stderr
```

An exception's traceback goes to `stderr` with `exit_code` 1 and the session carries on. `ended` means the session's process died during the cell (it exited, or was stopped for memory). One cell runs at a time per session. Full saves and pauses keep sessions like any other process; a quick save keeps files only. Sessions need a runtime that keeps processes between commands (gVisor).

## Saves

| Call | What it does |
| --- | --- |
| `ws.save()` | Quick save: the workspace's files |
| `ws.save("full")` | Full save: memory and running processes too; the workspace stops until resumed (needs the gVisor runtime) |
| `save.resume()` | Puts the save's own workspace back to exactly the saved state |
| `save.copy(n)` | `n` new workspaces, each starting from the save |
| `ws.pause()` | Frees memory now; the next command wakes the workspace with its processes |

## Files

`write_file` takes bytes, text, an open binary file or a local path. `read_file` returns bytes. `download_file` streams a large file to disk without holding it in memory.

## Retries and errors

- **Safe retries:** after a dropped connection, the client retries what is safe to repeat: reads, writes and deletes.
- **No duplicates:** `create_workspace`, `save` and `copy` send an idempotency key, reused by their retries. A retry after a lost reply returns the original instead of making a second one.
- **Never retried after a dropped connection:** commands and `resume`, because running them twice is not safe. A busy engine (`429`) did no work, so it is always retried.
- **Errors:** every engine error is a `cairn.CairnError` with the engine's stable `code`, the HTTP `status` and the `request_id` the engine logged. Each code has its own class: `NotFound`, `InvalidArgument`, `Conflict`, `SaveCorrupt`, `InsufficientStorage`, `TooManyRequests`, `UnsupportedByRuntime`, `Unauthorized` and `InternalError`. `ConnectionFailed` means the engine couldn't be reached, so the request may or may not have happened.

## Tests

```sh
python3 -m unittest discover -s sdk/python/tests   # builds and starts cairnd on the host runtime
```

The tests use the engine at `CAIRN_ENDPOINT` when set. The acceptance suite runs them as scenario G10.10 on every runtime, gVisor included.
