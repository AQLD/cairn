package acceptance

import (
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// G2.8, G17.4: what randomness copies keep. A generator inside a process is
// part of its memory, so every copy of a full save continues it exactly;
// the OS's randomness is fresh in each copy (G2.6).

// drawer starts a process that seeds Python's generator now, waits for a
// go-file, then writes three draws, one reseeded from the OS if asked.
const drawer = `nohup python3 -c "
import os, random, time
random.seed()  # from the OS, before the save
while not os.path.exists('/tmp/go'):
    time.sleep(0.05)
if os.path.exists('/tmp/reseed'):
    random.seed(os.urandom(16))
open('/tmp/draws.tmp', 'w').write(' '.join(str(random.random()) for _ in range(3)))
os.rename('/tmp/draws.tmp', '/tmp/draws')
" >/dev/null 2>&1 &`

func draws(e *Env, ws string, reseed bool) string {
	e.T.Helper()
	cmd := "touch /tmp/go"
	if reseed {
		cmd = "touch /tmp/reseed /tmp/go"
	}
	e.Sh(ws, cmd)
	deadline := time.Now().Add(20 * time.Second)
	for {
		out := strings.TrimSpace(e.Sh(ws, "cat /tmp/draws 2>/dev/null || true"))
		if out != "" {
			return out
		}
		if time.Now().After(deadline) {
			e.T.Fatalf("%s: the drawing process wrote nothing", ws)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// branches makes a full save of a workspace whose drawer is waiting, and n copies.
func branches(e *Env, n int) (string, []client.Workspace) {
	e.T.Helper()
	src := e.Workspace("experiment")
	e.Sh(src.ID, drawer)
	time.Sleep(time.Second) // seeded and waiting
	sv, err := e.C.Save(e.Ctx, src.ID, client.FullSave)
	e.Must(err)
	copies, err := e.C.Copy(e.Ctx, sv.ID, n)
	e.Must(err)
	e.Resume(sv.ID)
	return src.ID, copies
}

func init() {
	register("G2.8", func(e *Env) {
		src, copies := branches(e, 2)
		want := draws(e, src, false)
		for _, c := range copies {
			Equal(e, c.ID+"'s draws", draws(e, c.ID, false), want)
		}
		// while the OS's randomness differs in each
		seen := map[string]bool{}
		for _, id := range append([]string{src}, copies[0].ID, copies[1].ID) {
			r := strings.TrimSpace(e.Sh(id, "head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \\n'"))
			if seen[r] {
				e.T.Fatal("two copies read the same OS randomness")
			}
			seen[r] = true
		}
	})

	register("G17.4", func(e *Env) {
		// identical branches: replaying the same episode in several copies
		src, same := branches(e, 2)
		want := draws(e, src, false)
		for _, c := range same {
			Equal(e, "identical branch "+c.ID, draws(e, c.ID, false), want)
		}
		// independent branches: each copy reseeds from the OS first
		_, indep := branches(e, 3)
		got := map[string]bool{}
		for _, c := range indep {
			d := draws(e, c.ID, true)
			if got[d] {
				e.T.Fatal("two independent branches drew the same numbers after reseeding")
			}
			got[d] = true
		}
	})
}
