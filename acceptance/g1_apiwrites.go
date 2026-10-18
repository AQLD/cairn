package acceptance

import "strings"

func init() {
	// G1.13: the file API writes the workspace's folder from outside the
	// sandbox. Programs inside must see the new size, not one cached from an
	// earlier read: tools that read exactly st_size bytes (Python, wc -c)
	// would otherwise get a truncated or padded file.
	register("G1.13", func(e *Env) {
		ws := e.Workspace("api-writes")
		size := func() string { return strings.TrimSpace(e.Sh(ws.ID, "wc -c < note.txt")) }

		e.Write(ws.ID, "note.txt", "one\n")
		Equal(e, "size inside after the first write", size(), "4")
		Equal(e, "content inside", e.Sh(ws.ID, "cat note.txt"), "one\n")

		e.Write(ws.ID, "note.txt", "two, and longer\n")
		Equal(e, "size inside after the file grew", size(), "16")
		Equal(e, "content inside after the file grew", e.Sh(ws.ID, "cat note.txt"), "two, and longer\n")

		e.Write(ws.ID, "note.txt", "3\n")
		Equal(e, "size inside after the file shrank", size(), "2")
		Equal(e, "content inside after the file shrank", e.Sh(ws.ID, "cat note.txt"), "3\n")
	})
}
