"""A small reinforcement-learning environment, as a training run would put in
a workspace: an expensive one-time setup, then episodes of steps.

    python3 env.py setup            build the world (slow, run once)
    python3 env.py serve PORT       serve it over HTTP, state in memory
    python3 env.py episode ACTIONS  play an episode on the world kept on disk
"""

import http.server
import json
import os
import socket
import sys
import time
import urllib.parse

WORLD, STATE = "world.json", "state.json"


def setup():
    time.sleep(1)  # stands in for compiling a simulator or downloading assets
    world = {"size": 16, "goal": 13, "walls": [3, 7, 11]}
    with open(WORLD, "w") as f:
        json.dump(world, f)
    with open(STATE, "w") as f:
        json.dump({"pos": 0, "steps": 0, "log": []}, f)
    with open("setup-runs", "a") as f:
        f.write("1\n")


def step(world, state, action):
    nxt = max(0, min(world["size"] - 1, state["pos"] + action))
    if nxt not in world["walls"]:
        state["pos"] = nxt
    state["steps"] += 1
    done = state["pos"] == world["goal"]
    return {"obs": state["pos"], "reward": 1.0 if done else -0.01, "done": done}


def serve(port):
    world = json.load(open(WORLD))
    state = {"pos": 0, "steps": 0}  # lives in this process's memory only

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            url = urllib.parse.urlparse(self.path)
            q = urllib.parse.parse_qs(url.query)
            if url.path == "/step":
                body = step(world, state, int(q["action"][0]))
            else:
                body = {"pid": os.getpid(), "steps": state["steps"], "pos": state["pos"],
                        "hostname": socket.gethostname()}
            data = json.dumps(body).encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *args):
            pass

    http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()


def episode(actions):
    world = json.load(open(WORLD))
    state = json.load(open(STATE))
    for a in actions:
        r = step(world, state, a)
        state["log"].append(r)
        if r["done"]:
            break
    with open(STATE, "w") as f:
        json.dump(state, f)
    with open("episode.out", "w") as f:
        f.write(f"{state['steps']} steps, at {state['pos']}\n")
    print(json.dumps({"steps": state["steps"], "pos": state["pos"]}))


if __name__ == "__main__":
    cmd = sys.argv[1]
    if cmd == "setup":
        setup()
    elif cmd == "serve":
        serve(int(sys.argv[2]))
    elif cmd == "episode":
        episode([int(a) for a in sys.argv[2].split(",")])
