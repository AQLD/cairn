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
