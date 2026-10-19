# A Python session: one interpreter that runs cells in the same globals, so
# variables, imports and loaded data carry over between calls. Started by the
# engine as an ordinary process in the workspace (so a full save keeps it).
#
# Protocol, in the session's folder D (see sessions.go):
#   D/go       FIFO; a run writes the cell's number N once D/cell holds its code
#   D/out.N    the cell's stdout; D/err.N its stderr
#   D/running  holds N while the cell runs
#   D/done     FIFO; "N STATUS" when the cell has finished
# SIGINT interrupts the running cell (KeyboardInterrupt), never the session.
import ast
import linecache
import os
import signal
import sys
import traceback

d = sys.argv[1]
g = {'__name__': '__main__', '__builtins__': __builtins__}
busy = False
done = os.open(d + '/done', os.O_RDWR)  # held open: writing never blocks


def interrupt(signum, frame):
    if busy:
        raise KeyboardInterrupt


signal.signal(signal.SIGINT, interrupt)


def run(n, code):
    name = '<cell %s>' % n
    linecache.cache[name] = (len(code), None, code.splitlines(True), name)
    tree = ast.parse(code, name, 'exec')
    last = None
    if tree.body and isinstance(tree.body[-1], ast.Expr):
        last = ast.Expression(tree.body.pop().value)
    exec(compile(tree, name, 'exec'), g)
    if last is not None:
        value = eval(compile(last, name, 'eval'), g)
        if value is not None:
            print(repr(value))


while True:
    with open(d + '/go') as f:
        n = f.read().strip()
    if not n:
        continue
    with open(d + '/cell') as f:
        code = f.read()
    with open(d + '/running', 'w') as f:
        f.write(n)
    out = os.open('%s/out.%s' % (d, n), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    err = os.open('%s/err.%s' % (d, n), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    saved = os.dup(1), os.dup(2)
    os.dup2(out, 1)
    os.dup2(err, 2)
    status = 0
    try:
        busy = True
        run(n, code)
    except SystemExit as e:  # exit() ends the cell, not the session
        status = e.code if isinstance(e.code, int) else (0 if e.code is None else 1)
    except BaseException:
        traceback.print_exc()
        status = 1
    finally:
        busy = False
        sys.stdout.flush()
        sys.stderr.flush()
        os.dup2(saved[0], 1)
        os.dup2(saved[1], 2)
        for fd in (out, err, *saved):
            os.close(fd)
    os.remove(d + '/running')
    os.write(done, ('%s %d\n' % (n, status)).encode())
