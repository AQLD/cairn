//go:build linux

package disk

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// MountOverlay mounts at dir an overlay of the read-only folder lower with
// the writable folder upper (work is overlayfs's scratch folder, on the same
// filesystem as upper). Folder renames are made to work (redirect_dir=on);
// without it, renaming a folder that came from lower fails with EXDEV, which
// programs do not expect from a plain disk.
func MountOverlay(lower, upper, work, dir string) error {
	for _, p := range []string{lower, upper, work, dir} {
		if strings.ContainsAny(p, ",:") {
			return fmt.Errorf("overlay path %q contains ',' or ':'", p)
		}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,redirect_dir=on", lower, upper, work)
	if err := unix.Mount("overlay", dir, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay at %s: %w", dir, err)
	}
	return nil
}

// OverlaySupported reports nil if overlays can be mounted here (as
// MountOverlay does, with folder renames working), or why not. It mounts a
// small one under scratch, a folder on the filesystem that will hold upper
// folders, and removes it.
func OverlaySupported(scratch string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%w: needs root", ErrUnsupported)
	}
	tmp, err := os.MkdirTemp(scratch, ".overlay-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	dirs := map[string]string{}
	for _, n := range []string{"lower", "upper", "work", "merged"} {
		dirs[n] = filepath.Join(tmp, n)
		if err := os.Mkdir(dirs[n], 0o755); err != nil {
			return err
		}
	}
	if err := os.Mkdir(filepath.Join(dirs["lower"], "d"), 0o755); err != nil {
		return err
	}
	if err := MountOverlay(dirs["lower"], dirs["upper"], dirs["work"], dirs["merged"]); err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	defer Unmount(dirs["merged"])
	// a folder from lower must be renameable
	if err := os.Rename(filepath.Join(dirs["merged"], "d"), filepath.Join(dirs["merged"], "e")); err != nil {
		if errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("%w: overlay cannot rename folders (redirect_dir)", ErrUnsupported)
		}
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	return nil
}

// OverlayReportsWriteErrors reports nil if a write that fails underneath an
// overlay whose upper layer is on a disk image (as a workspace's is) reaches
// the program's fsync as an error, or why not. Some kernels (Linux 7.0) drop
// it: the program is told its data is safe while the kernel logs data loss.
//
// It reproduces that small, twice, each on its own: a 32 MB sparse image on
// an 8 MB tmpfs, mounted, and a 16 MB write and fsync that must fail - once
// straight on the image (if that does not fail, the probe proves nothing and
// says so), once through an overlay whose upper layer is on the image.
func OverlayReportsWriteErrors(scratch string) error {
	if failedWriteReported(scratch, false) != nil {
		// not a verdict on overlays: the setup itself could not show an error
		return fmt.Errorf("%w: write-error probe inconclusive (a failed write was not reported even without an overlay)", ErrUnsupported)
	}
	if err := failedWriteReported(scratch, true); err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	return nil
}

// failedWriteReported fills a sparse image's backing volume with a write
// (through an overlay on the image, with overlay) and returns nil if fsync
// reported the failure.
func failedWriteReported(scratch string, overlay bool) error {
	tmp, err := os.MkdirTemp(scratch, ".fsync-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	host, layer, lower, merged := filepath.Join(tmp, "host"), filepath.Join(tmp, "layer"), filepath.Join(tmp, "lower"), filepath.Join(tmp, "merged")
	for _, d := range []string{host, layer, lower, merged} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return err
		}
	}
	if err := unix.Mount("tmpfs", host, "tmpfs", 0, "size=8m"); err != nil {
		return fmt.Errorf("probe tmpfs: %w", err)
	}
	defer Unmount(host)
	img := filepath.Join(host, "probe.img")
	if err := Create(img, MinSizeMB); err != nil {
		return fmt.Errorf("probe image: %w", err)
	}
	if err := Mount(img, layer); err != nil {
		return fmt.Errorf("probe mount: %w", err)
	}
	defer Unmount(layer)
	target := filepath.Join(layer, "f")
	if overlay {
		for _, d := range []string{"upper", "work"} {
			if err := os.Mkdir(filepath.Join(layer, d), 0o755); err != nil {
				return err
			}
		}
		if err := MountOverlay(lower, filepath.Join(layer, "upper"), filepath.Join(layer, "work"), merged); err != nil {
			return err
		}
		defer Unmount(merged)
		target = filepath.Join(merged, "f")
	}
	if writeAndSync(target) == nil {
		return errors.New("a write that failed underneath was reported to fsync as success (a full volume under a workspace would lose data silently)")
	}
	return nil
}

// writeAndSync writes 16 MB to a new file and syncs it, returning the first error.
func writeAndSync(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i*31 + 7) // not zeros: nothing may skip writing them
	}
	for range 16 {
		if _, err := f.Write(buf); err != nil {
			return err
		}
	}
	return f.Sync()
}
