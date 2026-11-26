package save

import (
	"archive/tar"
	"io/fs"
	"os"
	"sync"
	"time"
)

// Extracting is mostly syscalls (create, write, chown, chmod, set times),
// several per file, and a template holds thousands of small files. The tar
// stream must be read in order, but its files can be written in parallel:
// fileWriters writes small files on a few goroutines while the reader goes on.
// Folders are still made in order by the reader, before any file in them is
// queued; anything that needs earlier files on disk (a hard link to one, a
// removal in a delta) waits for the queue to drain first.

const (
	writerCount   = 4
	inlineMax     = 1 << 20  // bigger files are written by the reader itself
	inFlightBytes = 32 << 20 // file content queued at once
)

type fileJob struct {
	name  string
	data  []byte
	uid   int
	gid   int
	mode  fs.FileMode
	mtime time.Time
}

type fileWriters struct {
	root   *os.Root
	asRoot bool
	jobs   chan fileJob
	room   chan struct{}  // one token per byte budget unit (64 KiB)
	wg     sync.WaitGroup // queued jobs not yet written
	done   sync.WaitGroup // the goroutines
	mu     sync.Mutex
	err    error
	closed sync.Once
}

const budgetUnit = 64 << 10

func newFileWriters(root *os.Root, asRoot bool) *fileWriters {
	w := &fileWriters{root: root, asRoot: asRoot, jobs: make(chan fileJob, 64), room: make(chan struct{}, inFlightBytes/budgetUnit)}
	for range cap(w.room) {
		w.room <- struct{}{}
	}
	for range writerCount {
		w.done.Add(1)
		go w.loop()
	}
	return w
}

func units(n int) int { return max(1, (n+budgetUnit-1)/budgetUnit) }

// add queues a file, waiting while the byte budget is used up.
func (w *fileWriters) add(j fileJob) error {
	if err := w.failed(); err != nil {
		return err
	}
	for range units(len(j.data)) {
		<-w.room
	}
	w.wg.Add(1)
	w.jobs <- j
	return nil
}

func (w *fileWriters) loop() {
	defer w.done.Done()
	for j := range w.jobs {
		if w.failed() == nil {
			if err := writeFile(w.root, w.asRoot, j); err != nil {
				w.mu.Lock()
				if w.err == nil {
					w.err = err
				}
				w.mu.Unlock()
			}
		}
		for range units(len(j.data)) {
			w.room <- struct{}{}
		}
		w.wg.Done()
	}
}

func (w *fileWriters) failed() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// drain waits until every queued file is on disk.
func (w *fileWriters) drain() error {
	w.wg.Wait()
	return w.failed()
}

// close drains and stops the goroutines; later calls only report.
func (w *fileWriters) close() error {
	err := w.drain()
	w.closed.Do(func() {
		close(w.jobs)
		w.done.Wait()
	})
	return err
}

// writeFile creates one file with its content, owner, mode and times.
func writeFile(root *os.Root, asRoot bool, j fileJob) error {
	f, err := root.OpenFile(j.name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(j.data)
	// owner then mode on the open file (chown clears setuid and setgid)
	if err == nil && asRoot {
		err = f.Chown(j.uid, j.gid)
	}
	if err == nil {
		err = f.Chmod(j.mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// keep modification times: a restored filesystem must look like the one
	// a checkpoint was taken against
	_ = root.Chtimes(j.name, j.mtime, j.mtime)
	return nil
}

// jobFor reads a small file's content from the stream into a job.
func jobFor(tr *tar.Reader, name string, hdr *tar.Header, mode fs.FileMode) (fileJob, error) {
	data := make([]byte, hdr.Size)
	if _, err := ioReadFull(tr, data); err != nil {
		return fileJob{}, err
	}
	return fileJob{name: name, data: data, uid: hdr.Uid, gid: hdr.Gid, mode: mode, mtime: hdr.ModTime}, nil
}
