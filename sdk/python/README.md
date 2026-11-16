# Cairn Python SDK

A Python client for [Cairn](https://github.com/AQLD/cairn). Cairn gives each AI agent a private Linux workspace it can save, resume and copy. The client uses only the standard library; Python 3.9 or newer.

```sh
pip install ./sdk/python        # from a checkout of the repository
```

```python
import cairn

client = cairn.Client()   # the engine `cairn up` started; or Client(url, token=...), CAIRN_ENDPOINT / CAIRN_TOKEN

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

## Activity log

Every workspace keeps a log of what was done in it: commands with their exit code, duration and the end of their output, files written and read, saves, resumes, copies, pauses and session cells. It is how a person sees what an agent did:

```python
for ev in ws.events(last=20):                  # or kind="exec", after=<id>
    print(ev.time, ev.kind, ev.summary)        # "exec: pytest -q → exit 1 in 2.3s"
    ev.data.get("exit_code"), ev.data.get("stderr_tail")

for ev in ws.follow_events():                  # what is there, then each new event live
    print(ev.summary)
```

From a terminal: `cairn log WORKSPACE -f -v`. Environment variable values are never logged, only their names; command lines and output tails are, so keep secrets out of arguments. Each workspace keeps its newest 10,000 events; a deleted workspace's log is kept for the save retention window (7 days by default).

## Saves

| Call | What it does |
| --- | --- |
| `ws.save()` | Quick save: the workspace's files |
| `ws.save("full")` | Full save: memory and running processes too; the workspace stops until resumed (needs the gVisor runtime) |
| `ws.save("full", keep_running=True)` | Full save, and the workspace carries on with its processes still running: save before a risky step, keep working, resume the save if the step goes wrong |
| `save.resume()` | Puts the save's own workspace back to exactly the saved state |
| `save.copy(n)` | `n` new workspaces, each starting from the save |
| `ws.pause()` | Frees memory now; the next command wakes the workspace with its processes |

## Files

`write_file` takes bytes, text, an open binary file or a local path. `read_file` returns bytes. `download_file` streams a large file to disk without holding it in memory.

Whole folders move with `push` and `pull`, which merge with what is already there and keep modes, times, symlinks and empty folders:

```python
ws.push("./my-project", "/root/my-project", exclude=["node_modules", ".venv", "*.pyc"])
ws.run("cd /root/my-project && pytest -q")
ws.pull("/root/my-project", "./my-project")   # the agent's changes, back on your machine
```

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
