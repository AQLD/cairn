// Package save defines Cairn's on-disk save format and writes, verifies and
// reads saves.
//
// A save is one folder:
//
//	<save-id>/
//	  manifest.json   versioned description of the save, with a checksum per file
//	  rootfs.tar      the workspace filesystem
//	  memory/         memory and process state (full saves only): the files the
//	                  runtime's checkpoint wrote (for gVisor: checkpoint.img,
//	                  pages.img, pages_meta.img), each listed in the manifest
//
// Saves are built in a temporary folder next to the final one and renamed into
// place only when complete and synced, so a crash never leaves a half-written
// save that looks usable. Changing this format breaks old saves: bump
// FormatVersion and keep a reader for every older version.
package save

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FormatVersion is the version written into every new manifest.
const FormatVersion = 1

const (
	ManifestFile = "manifest.json"
	RootfsFile   = "rootfs.tar"
	MemoryDir    = "memory"
	tmpPrefix    = ".tmp-"
)

// Kind is what a save contains.
type Kind string

const (
	// Quick saves the filesystem while the workspace keeps running.
	Quick Kind = "quick"
	// Full also saves memory and processes, then frees the server.
	Full Kind = "full"
)

// ErrCorrupt means a save failed verification and must not be restored.
var ErrCorrupt = errors.New("save is corrupt")

// File is one file in a save, with its size and SHA-256.
type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest describes a save. Everything needed to decide whether a save can be
// restored on a given server is recorded here.
type Manifest struct {
	FormatVersion int       `json:"format_version"`
	SaveID        string    `json:"save_id"`
	WorkspaceID   string    `json:"workspace_id"`
	Kind          Kind      `json:"kind"`
	CreatedAt     time.Time `json:"created_at"`
	// Runtime that made the save; full saves restore only with the same runtime build.
	Runtime        string `json:"runtime"`
	RuntimeVersion string `json:"runtime_version"`
	// CPUProfile is the CPU feature baseline a full save is pinned to.
	CPUProfile string `json:"cpu_profile,omitempty"`
	Files      []File `json:"files"`
	// Workspace describes the workspace saved, so another server can make
	// copies of the save without this one's records (saves in object storage).
	// Absent in saves made before it existed.
	Workspace *WorkspaceInfo `json:"workspace,omitempty"`
}

// WorkspaceInfo is what a copy of a save needs to know about its workspace.
type WorkspaceInfo struct {
	Name string          `json:"name"`
	Spec json.RawMessage `json:"spec"`
}

// Size is the total size of the files in the save.
func (m *Manifest) Size() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// Builder writes a new save into a temporary folder. Call Commit to publish it
// or Abort to throw it away.
type Builder struct {
	root     string // folder holding all saves
	tmp      string // temporary folder being written
	manifest Manifest
	done     bool
}

// NewBuilder starts a save under root (the folder that holds all saves).
func NewBuilder(root string, m Manifest) (*Builder, error) {
	if m.SaveID == "" || strings.ContainsAny(m.SaveID, `/\`) || strings.HasPrefix(m.SaveID, ".") {
		return nil, fmt.Errorf("invalid save id %q", m.SaveID)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(root, tmpPrefix+m.SaveID+"-")
	if err != nil {
		return nil, err
	}
	m.FormatVersion = FormatVersion
	m.Files = nil
	return &Builder{root: root, tmp: tmp, manifest: m}, nil
}

// Dir is the temporary folder, for runtimes that write files themselves
// (for example a memory image). Record them with AddFile.
func (b *Builder) Dir() string { return b.tmp }

// WriteFile streams r into a new file in the save and records its checksum.
func (b *Builder) WriteFile(name string, r io.Reader) error {
	if err := validName(name); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(b.tmp, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	b.manifest.Files = append(b.manifest.Files, File{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
	return nil
}

// MemoryDir creates and returns the folder a runtime writes a checkpoint into.
// Record its files with AddMemory once the runtime is done.
func (b *Builder) MemoryDir() (string, error) {
	dir := filepath.Join(b.tmp, MemoryDir)
	return dir, os.MkdirAll(dir, 0o700)
}

// AddMemory records every file the runtime wrote into MemoryDir.
func (b *Builder) AddMemory() error {
	entries, err := os.ReadDir(filepath.Join(b.tmp, MemoryDir))
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return errors.New("runtime wrote no checkpoint files")
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			return fmt.Errorf("unexpected %s in checkpoint", e.Name())
		}
		if err := b.AddFile(MemoryDir + "/" + e.Name()); err != nil {
			return err
		}
	}
	return syncDir(filepath.Join(b.tmp, MemoryDir))
}

// HasMemory reports whether the save holds memory and process state.
func (m *Manifest) HasMemory() bool {
	for _, f := range m.Files {
		if strings.HasPrefix(f.Name, MemoryDir+"/") {
			return true
		}
	}
	return false
}

// AddFile records a file that something else already wrote into Dir.
func (b *Builder) AddFile(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	sum, size, err := hashFile(filepath.Join(b.tmp, filepath.FromSlash(name)))
	if err != nil {
		return err
	}
	b.manifest.Files = append(b.manifest.Files, File{Name: name, Size: size, SHA256: sum})
	return nil
}

// Commit writes the manifest, syncs everything and atomically renames the save
// into place. Until Commit returns, the save does not exist.
func (b *Builder) Commit() (*Manifest, error) {
	if b.done {
		return nil, errors.New("save already committed or aborted")
	}
	data, err := json.MarshalIndent(b.manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeSynced(filepath.Join(b.tmp, ManifestFile), data); err != nil {
		return nil, err
	}
	if err := syncDir(b.tmp); err != nil {
		return nil, err
	}
	final := filepath.Join(b.root, b.manifest.SaveID)
	if err := os.Rename(b.tmp, final); err != nil {
		return nil, err
	}
	if err := syncDir(b.root); err != nil {
		return nil, err
	}
	b.done = true
	m := b.manifest
	return &m, nil
}

// Abort deletes the temporary folder. Safe to call after Commit (it does nothing).
func (b *Builder) Abort() {
	if !b.done {
		b.done = true
		_ = os.RemoveAll(b.tmp)
	}
}

// ReadManifest reads a save's manifest without checking its files.
func ReadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: manifest missing", ErrCorrupt)
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: manifest unreadable: %v", ErrCorrupt, err)
	}
	if m.FormatVersion < 1 || m.FormatVersion > FormatVersion {
		return nil, fmt.Errorf("unsupported save format version %d (this engine reads up to %d)", m.FormatVersion, FormatVersion)
	}
	return &m, nil
}

// Verify reads the manifest and checks every file's size and checksum. A save
// must pass Verify before anything is restored from it.
func Verify(dir string) (*Manifest, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	for _, f := range m.Files {
		if err := validName(f.Name); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		sum, size, err := hashFile(filepath.Join(dir, filepath.FromSlash(f.Name)))
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, f.Name, err)
		}
		if size != f.Size || sum != f.SHA256 {
			return nil, fmt.Errorf("%w: %s does not match its checksum", ErrCorrupt, f.Name)
		}
	}
	return m, nil
}

// CleanupTemp removes half-written saves left behind by a crash.
func CleanupTemp(root string) (removed int, err error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), tmpPrefix) {
			if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// validName allows a plain file name, or one inside the memory folder:
// nothing that could point outside the save.
func validName(name string) error {
	base := strings.TrimPrefix(name, MemoryDir+"/")
	if base == "" || base != filepath.Base(base) || base == "." || base == ".." || strings.Contains(base, "\\") {
		return fmt.Errorf("invalid file name %q", name)
	}
	return nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
