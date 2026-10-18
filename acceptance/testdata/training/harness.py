"""A training harness driving Cairn through the Python SDK, as a reinforcement
learning or evaluation run would. Run by the acceptance suite (G17):

    python3 harness.py copy-live-setup | reset-between-episodes

Exits non-zero, with the reason on stderr, if the environment misbehaves.
Prints one JSON line of measurements on success.
"""

import concurrent.futures
import json
import os
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "..", "sdk", "python"))
import cairn  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
ENV_PY = open(os.path.join(HERE, "env.py")).read()
PORT = 9000
COPIES = 8

client = cairn.Client()  # CAIRN_ENDPOINT / CAIRN_TOKEN


def check(ok, why):
    if not ok:
        print(why, file=sys.stderr)
        sys.exit(1)


def get(ws, path):
    """GET a path on the environment server inside the workspace."""
    out = ws.run("python3", "-c",
                 f"import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:{PORT}{path}', timeout=5).read().decode())")
    return json.loads(out)


def wait_for_server(ws):
    for _ in range(100):
        try:
            return get(ws, "/state")
        except cairn.CommandFailed:
            time.sleep(0.1)
    check(False, "environment server never answered: " + ws.read_file("serve.log").decode())


def new_env(name):
    ws = client.create_workspace(name)
    ws.write_file("env.py", ENV_PY)
    start = time.time()
    ws.run("python3", "env.py", "setup")
    return ws, time.time() - start


def copy_live_setup():
    """G17.1: setup runs once; environments copy from its live state."""
    ws, setup_s = new_env("train-live")
    ws.exec("nohup python3 env.py serve %d > serve.log 2>&1 &" % PORT)
    wait_for_server(ws)
    get(ws, "/step?action=1")  # warm: one step taken, held in the server's memory
    warm = get(ws, "/state")
    check(warm["steps"] == 1, f"warm-up step not recorded: {warm}")

    save = ws.save("full")
    start = time.time()
    envs = save.copy(COPIES)
    copy_s = time.time() - start
    check(len(envs) == COPIES, f"asked for {COPIES} copies, got {len(envs)}")

    def episode(i, env):
        try:
            st = get(env, "/state")
        except cairn.CommandFailed as e:
            check(False, f"copy {i}: the environment server is not running in the copy: {e}")
        check(st["pid"] == warm["pid"], f"copy {i}: server pid {st['pid']}, want the warm one {warm['pid']}")
        check(st["steps"] == 1, f"copy {i}: server memory lost: {st}")
        check(env.read_file("setup-runs") == b"1\n", f"copy {i}: setup ran again")
        for _ in range(i + 1):  # each environment plays a different episode length
            get(env, "/step?action=2")
        return get(env, "/state")

    with concurrent.futures.ThreadPoolExecutor(COPIES) as pool:
        finals = list(pool.map(lambda a: episode(*a), enumerate(envs)))
    for i, st in enumerate(finals):
        check(st["steps"] == 1 + i + 1, f"copy {i}: {st['steps']} steps, want {i + 2}: environments are not independent")
    hostnames = {st["hostname"] for st in finals}
    check(len(hostnames) == COPIES, f"copies share a hostname: {sorted(hostnames)}")

    for env in envs:
        env.delete()
    ws.delete()
    return {"setup_s": round(setup_s, 2), "copies": COPIES, "copy_s": round(copy_s, 2)}


def reset_between_episodes():
    """G17.2: an environment resets to its start between episodes."""
    ws, _ = new_env("train-reset")
    start_state = ws.read_file("state.json")
    start = ws.save()
    resets = []
    for actions in ("1,1,1", "2,2,2,2,2,2", "1,2,1,2,5,5"):
        out = json.loads(ws.run("python3", "env.py", "episode", actions))
        check(out["steps"] > 0, f"episode {actions} took no steps")
        check(ws.read_file("state.json") != start_state, f"episode {actions} did not change the state")
        t = time.time()
        start.resume()
        resets.append(time.time() - t)
        check(ws.read_file("state.json") == start_state, f"after episode {actions}: state not reset")
        try:
            ws.read_file("episode.out")
            check(False, f"after episode {actions}: the episode's output survived the reset")
        except cairn.NotFound:
            pass
        check(ws.read_file("setup-runs") == b"1\n", "setup ran again")
    ws.delete()
    return {"episodes": len(resets), "reset_max_s": round(max(resets), 3)}


if __name__ == "__main__":
    scenario = {"copy-live-setup": copy_live_setup, "reset-between-episodes": reset_between_episodes}[sys.argv[1]]
    print(json.dumps(scenario()))
