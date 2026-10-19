package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	sysunix "golang.org/x/sys/unix"

	"github.com/AQLD/cairn/internal/engine"
)

// bigFileBytes is the size G19.5.1 moves: past the old 1 GiB cap in -short
// mode, several gigabytes otherwise (CAIRN_BIGFILE_MB to choose).
func bigFileBytes() int64 {
	if mb, err := strconv.Atoi(os.Getenv("CAIRN_BIGFILE_MB")); err == nil && mb > 0 {
		return int64(mb) << 20
	}
	if testing.Short() {
		return 1536 << 20
	}
	return 4 << 30
}

// countingHash hashes and counts what is written to it.
type countingHash struct {
	h interface {
		io.Writer
		Sum([]byte) []byte
	}
	n int64
}

func newCountingHash() *countingHash { return &countingHash{h: sha256.New()} }

func (c *countingHash) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return c.h.Write(p)
}

func (c *countingHash) sum() string { return hex.EncodeToString(c.h.Sum(nil)) }

func freeBytes(dir string) (uint64, error) {
	var st sysunix.Statfs_t
	if err := sysunix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func init() {
	register("G19.5.1", func(e *Env) {
		ws := e.Workspace("big-file")
		size := bigFileBytes()

		// the content is generated as it is sent: nothing here holds the file either
		up := newCountingHash()
		src := io.TeeReader(io.LimitReader(rand.NewChaCha8([32]byte{19, 5}), size), up)
		start := time.Now()
		e.Must(e.C.WriteFileFrom(e.Ctx, ws.ID, "data/big.bin", src))
		upTime := time.Since(start)
		Equal(e, "bytes sent", up.n, size)
		Equal(e, "size inside the workspace", strings.TrimSpace(e.Sh(ws.ID, "wc -c < data/big.bin")), strconv.FormatInt(size, 10))

		down := newCountingHash()
		start = time.Now()
		e.Must(e.C.ReadFileTo(e.Ctx, ws.ID, "data/big.bin", down))
		downTime := time.Since(start)
		Equal(e, "bytes downloaded", down.n, size)
		Equal(e, "content comes back byte for byte", down.sum(), up.sum())
		mib := float64(size) / (1 << 20)
		e.T.Logf("%.0f MiB: in at %.0f MiB/s, out at %.0f MiB/s", mib, mib/upTime.Seconds(), mib/downTime.Seconds())
	})

	register("G19.5.2", func(e *Env) {
		// Plain folders (no disk image): the case the engine's own guard covers.
		// Leave only ~150 MB above the headroom, then send 400 MB.
		e.RestartWith(func(c *engine.Config) { c.NoDiskImages = true })
		ws := e.Workspace("headroom") // first: creating it uses space (its template)
		free, err := freeBytes(e.DataDir)
		e.Must(err)
		e.RestartWith(func(c *engine.Config) {
			c.NoDiskImages = true
			c.MinFreeBytes = int64(free) - 150<<20
		})
		err = e.C.WriteFileFrom(e.Ctx, ws.ID, "fill.bin", io.LimitReader(rand.NewChaCha8([32]byte{1}), 400<<20))
		e.ErrCode(err, "insufficient_storage")
		_, err = e.C.ReadFile(e.Ctx, ws.ID, "fill.bin")
		e.ErrCode(err, "not_found")
		after, err := freeBytes(e.DataDir)
		e.Must(err)
		if int64(after) < int64(free)-100<<20 {
			e.T.Fatalf("the refused upload kept its space: %d MB free before, %d MB after", free>>20, after>>20)
		}
		// the workspace still works
		e.Write(ws.ID, "small.txt", "fine\n")
		Equal(e, "small write after the refusal", e.Read(ws.ID, "small.txt"), "fine\n")
	})
}
