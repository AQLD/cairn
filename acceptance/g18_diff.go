package acceptance

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AQLD/cairn/client"
)

// G18.4: the files changed between two saves can be listed, read from the
// saves themselves (nothing is restored).

// diffLines renders a diff's changes under prefix as "A|M|D type path" lines,
// sorted, and checks the counts against the whole list.
func diffLines(e *Env, d *client.SaveDiff, prefix string) []string {
	e.T.Helper()
	counts := map[string]int{}
	var out []string
	for _, c := range d.Changes {
		counts[c.Change]++
		if strings.HasPrefix(c.Path, prefix) {
			out = append(out, fmt.Sprintf("%s %s %s", map[string]string{"added": "A", "modified": "M", "removed": "D"}[c.Change], c.Type, c.Path))
		}
	}
	if !d.Truncated && (counts["added"] != d.Added || counts["modified"] != d.Modified || counts["removed"] != d.Removed) {
		e.T.Fatalf("counts %d/%d/%d do not match the %d changes listed (%v)", d.Added, d.Modified, d.Removed, len(d.Changes), counts)
	}
	if !sort.SliceIsSorted(d.Changes, func(i, j int) bool { return d.Changes[i].Path < d.Changes[j].Path }) {
		e.T.Fatal("changes are not sorted by path")
	}
	sort.Strings(out)
	return out
}

func sameLines(e *Env, what string, got, want []string) {
	e.T.Helper()
	sort.Strings(want)
	if g, w := strings.Join(got, "\n"), strings.Join(want, "\n"); g != w {
		e.T.Fatalf("%s\n--- want\n%s\n--- got\n%s", what, w, g)
	}
}

func init() {
	register("G18.4.1", func(e *Env) {
		ws := e.Workspace("diff")
		e.Sh(ws.ID, `mkdir -p g18/tree/x g18/dir-to-file && cd g18 &&
echo one > edit.txt && echo bye > gone.txt && echo '#!/bin/sh' > run.sh &&
printf same > same.txt && echo deep > tree/x/y.txt && echo f > dir-to-file/f.txt &&
ln -s edit.txt link`)
		before := e.Save(ws.ID)
		e.Sh(ws.ID, `cd g18 &&
echo two >> edit.txt && rm gone.txt && chmod 755 run.sh &&
sleep 1 && printf same > same.txt &&
rm -rf tree && ln -sfn run.sh link &&
rm -rf dir-to-file && echo now a file > dir-to-file &&
mkdir -p new/deep && echo n > new/deep/n.txt`)
		after := e.Save(ws.ID)

		d, err := e.C.DiffSaves(e.Ctx, after.ID, before.ID, 0)
		e.Must(err)
		Equal(e, "diff from", d.From, before.ID)
		Equal(e, "diff to", d.To, after.ID)
		Equal(e, "truncated", d.Truncated, false)
		sameLines(e, "changes between the two saves", diffLines(e, d, "/g18/"), []string{
			"A dir /g18/new",
			"A dir /g18/new/deep",
			"A file /g18/new/deep/n.txt",
			"M file /g18/dir-to-file",
			"D file /g18/dir-to-file/f.txt",
			"M file /g18/edit.txt",
			"D file /g18/gone.txt",
			"M symlink /g18/link",
			"M file /g18/run.sh",
			"D dir /g18/tree",
			"D dir /g18/tree/x",
			"D file /g18/tree/x/y.txt",
		})
		for _, c := range d.Changes {
			if c.Path == "/g18/new/deep/n.txt" && c.Size != 2 {
				e.T.Fatalf("size of an added file: got %d, want 2", c.Size)
			}
		}

		// the other way round, every change flips
		back, err := e.C.DiffSaves(e.Ctx, before.ID, after.ID, 0)
		e.Must(err)
		Equal(e, "added one way = removed the other", back.Removed, d.Added)
		Equal(e, "removed one way = added the other", back.Added, d.Removed)
		Equal(e, "modified both ways", back.Modified, d.Modified)

		same, err := e.C.DiffSaves(e.Ctx, after.ID, after.ID, 0)
		e.Must(err)
		Equal(e, "changes between a save and itself", len(same.Changes), 0)
	})

	register("G18.4.2", func(e *Env) {
		ws := e.Workspace("diff-parent")
		e.Sh(ws.ID, `mkdir -p g18p`)
		first := e.Save(ws.ID)
		if first.Parent == "" {
			_, err := e.C.DiffSaves(e.Ctx, first.ID, "", 0)
			e.ErrCode(err, "invalid_argument")
		}

		e.Sh(ws.ID, `echo a > g18p/a.txt`)
		second := e.Save(ws.ID)
		Equal(e, "second save's parent", second.Parent, first.ID)
		d, err := e.C.DiffSaves(e.Ctx, second.ID, "", 0)
		e.Must(err)
		Equal(e, "default diff is against the parent", d.From, first.ID)
		sameLines(e, "changes since the parent", diffLines(e, d, "/g18p/"), []string{"A file /g18p/a.txt"})

		// rewind to the first save, then a different step: a branch
		e.Resume(first.ID)
		e.Sh(ws.ID, `echo b > g18p/b.txt`)
		branch := e.Save(ws.ID)
		d, err = e.C.DiffSaves(e.Ctx, branch.ID, "", 0)
		e.Must(err)
		Equal(e, "a branch diffs against the save rewound to", d.From, first.ID)
		sameLines(e, "changes on the branch", diffLines(e, d, "/g18p/"), []string{"A file /g18p/b.txt"})

		_, err = e.C.DiffSaves(e.Ctx, branch.ID, "sv_doesnotexist", 0)
		e.ErrCode(err, "not_found")
	})

	register("G18.4.3", func(e *Env) {
		deltaTemplate(e, "diff-delta")
		ws := templatedWorkspace(e, "diff-delta", "diff-delta")
		before := e.Save(ws.ID)
		e.Sh(ws.ID, agentEdits)
		after := e.Save(ws.ID)
		if after.Size >= 1<<20 {
			e.T.Fatalf("save of small edits is %d bytes: want a delta against the template", after.Size)
		}
		d, err := e.C.DiffSaves(e.Ctx, after.ID, before.ID, 0)
		e.Must(err)
		p := "/" + deltaDir + "/"
		sameLines(e, "agent's edits between two delta saves", diffLines(e, d, p), []string{
			"M file " + p + "same.txt", // same size, old time put back: only the content shows it
			"D dir " + p + "gone",
			"D dir " + p + "gone/x",
			"D file " + p + "gone/x/y.txt",
			"A file " + p + "lib/new.txt",
			"M file " + p + "run.sh",
			"M symlink " + p + "link",
			"M file " + p + "lib/c.txt",
			"A dir " + p + "added",
			"A dir " + p + "added/deep",
			"A file " + p + "added/deep/n.txt",
			"D file " + p + "lib/a.txt",
			"M file " + p + "keep/edit.txt",
		})
	})

	register("G18.4.4", func(e *Env) {
		ws := e.Workspace("diff-limit")
		before := e.Save(ws.ID)
		e.Sh(ws.ID, `mkdir g18lim && cd g18lim && for i in $(seq 10 39); do echo $i > f$i.txt; done`)
		after := e.Save(ws.ID)

		all, err := e.C.DiffSaves(e.Ctx, after.ID, before.ID, 0)
		e.Must(err)
		Equal(e, "truncated without a limit", all.Truncated, false)
		lines := diffLines(e, all, "/g18lim")
		Equal(e, "changes under g18lim", len(lines), 31)

		cut, err := e.C.DiffSaves(e.Ctx, after.ID, before.ID, 10)
		e.Must(err)
		Equal(e, "changes listed with limit 10", len(cut.Changes), 10)
		Equal(e, "truncated", cut.Truncated, true)
		Equal(e, "added counts every change", cut.Added, all.Added)
		for i := range cut.Changes {
			Equal(e, fmt.Sprintf("change %d is the same as without a limit", i), cut.Changes[i], all.Changes[i])
		}

		_, err = e.C.DiffSaves(e.Ctx, after.ID, before.ID, -1)
		e.Must(err) // 0 or less: the default
	})
}
