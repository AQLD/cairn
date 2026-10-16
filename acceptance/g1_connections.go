package acceptance

import (
	"encoding/json"
	"time"

	"github.com/AQLD/cairn/client"
)

// connPair is a server and a client inside one workspace, connected once over
// loopback TCP and once over a unix socket, exchanging pings forever. The client
// never reconnects: any break shows up as an error.
const connPair = `
import json, os, socket, sys, threading, time

def serve(sock):
    while True:
        conn, _ = sock.accept()
        def echo(c):
            while True:
                data = c.recv(64)
                if not data: return
                c.sendall(data)
        threading.Thread(target=echo, args=(conn,), daemon=True).start()

if sys.argv[1] == "server":
    t = socket.socket(); t.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    t.bind(("127.0.0.1", 7000)); t.listen()
    if os.path.exists("/root/db.sock"): os.remove("/root/db.sock")
    u = socket.socket(socket.AF_UNIX); u.bind("/root/db.sock"); u.listen()
    threading.Thread(target=serve, args=(u,), daemon=True).start()
    serve(t)
else:
    for _ in range(100):
        try:
            t = socket.create_connection(("127.0.0.1", 7000)); u = socket.socket(socket.AF_UNIX); u.connect("/root/db.sock"); break
        except OSError: time.sleep(0.1)
    st = {"tcp": 0, "unix": 0, "error": ""}
    while True:
        for name, s in (("tcp", t), ("unix", u)):
            try:
                s.sendall(b"ping"); assert s.recv(64) == b"ping"; st[name] += 1
            except Exception as e:
                st["error"] = "%s: %r" % (name, e)
        with open("/root/conn.tmp", "w") as f: json.dump(st, f)
        os.replace("/root/conn.tmp", "/root/conn.json")
        if st["error"]: sys.exit(1)
        time.sleep(0.05)
`

type connState struct {
	TCP   int    `json:"tcp"`
	Unix  int    `json:"unix"`
	Error string `json:"error"`
}

func readConn(e *Env, ws string) connState {
	e.T.Helper()
	var st connState
	for i := 0; i < 100; i++ {
		if out, err := e.C.ReadFile(e.Ctx, ws, "root/conn.json"); err == nil && json.Unmarshal(out, &st) == nil && st.TCP > 0 {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.T.Fatal("the connected pair never started")
	return st
}

func init() {
	register("G1.12", func(e *Env) {
		ws := e.Workspace("app-and-db")
		e.Must(e.C.WriteFile(e.Ctx, ws.ID, "root/pair.py", []byte(connPair)))
		background(e, ws.ID, "exec python3 /root/pair.py server")
		background(e, ws.ID, "exec python3 /root/pair.py client")
		before := readConn(e, ws.ID)

		sv, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		e.Resume(sv.ID)

		time.Sleep(500 * time.Millisecond)
		mid := readConn(e, ws.ID)
		time.Sleep(500 * time.Millisecond)
		after := readConn(e, ws.ID)
		if after.Error != "" {
			e.T.Fatalf("a connection inside the workspace broke across the save: %s", after.Error)
		}
		if after.TCP <= mid.TCP || after.Unix <= mid.Unix || mid.TCP < before.TCP {
			e.T.Fatalf("the pair stopped talking: before %+v, after resume %+v then %+v", before, mid, after)
		}
	})
}
