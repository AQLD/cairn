//go:build linux

package disk

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Supported reports nil if images can be used here, or why not.
func Supported() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%w: needs root", ErrUnsupported)
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		return fmt.Errorf("%w: mkfs.ext4 not found (install e2fsprogs)", ErrUnsupported)
	}
	f, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("%w: no loop devices: %v", ErrUnsupported, err)
	}
	f.Close()
	return nil
}

// Create makes a sparse image of sizeMB holding an empty ext4 filesystem.
// An existing file at img is replaced.
func Create(img string, sizeMB int64) error {
	if sizeMB < MinSizeMB {
		return fmt.Errorf("disk size %d MB is below the minimum of %d MB", sizeMB, MinSizeMB)
	}
	f, err := os.OpenFile(img, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = f.Truncate(sizeMB << 20)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// No reserved blocks (the whole size is the workspace's), lazy init so the
	// image stays sparse.
	out, err := exec.Command("mkfs.ext4", "-q", "-F", "-m", "0",
		"-E", "lazy_itable_init=1,lazy_journal_init=1", img).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Damaged reports whether the filesystem mounted at dir has hit errors. ext4
// records them (and stops writing: an aborted journal makes it read-only) when
// a write fails underneath it, for instance when the volume holding a sparse
// image runs out of space. The mount still says "rw", so this reads ext4's
// own error count.
func Damaged(dir string) (bool, error) {
	dev, err := mountSource(dir)
	if err != nil || dev == "" {
		return false, err
	}
	b, err := os.ReadFile(filepath.Join("/sys/fs/ext4", filepath.Base(dev), "errors_count"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // not ext4
	}
	if err != nil {
		return false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return n > 0, err
}

// mountSource is the device mounted at dir ("" if nothing is).
func mountSource(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		// id parent major:minor root mountpoint options ... - fstype source superoptions
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || unescape(fields[4]) != abs {
			continue
		}
		for i, f := range fields {
			if f == "-" && i+2 < len(fields) {
				return fields[i+2], nil
			}
		}
	}
	return "", sc.Err()
}

// Repair checks and fixes the unmounted image's filesystem (e2fsck -fy).
func Repair(img string) error {
	out, err := exec.Command("e2fsck", "-f", "-y", img).CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() < 4 {
		return nil // 1: errors fixed; 2: fixed, reboot advised (not for an image)
	}
	if err != nil {
		return fmt.Errorf("e2fsck: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Mount attaches img to a free loop device and mounts it at dir, which must
// exist. The loop device is released when the filesystem is unmounted.
func Mount(img, dir string) error {
	f, err := os.OpenFile(img, os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	loop, err := attachLoop(f)
	if err != nil {
		return fmt.Errorf("attach %s: %w", img, err)
	}
	// Keep the device open until mounted: with auto-clear, its last close
	// while unused detaches it.
	defer loop.Close()
	// discard: deleting files gives their space back to the host.
	// nodev: device files inside a workspace mean nothing to the host.
	if err := unix.Mount(loop.Name(), dir, "ext4", unix.MS_NODEV, "discard"); err != nil {
		_ = unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_CLR_FD, 0)
		return fmt.Errorf("mount %s at %s: %w", img, dir, err)
	}
	return nil
}

// attachLoop binds f to a free loop device and returns the device, open.
func attachLoop(f *os.File) (*os.File, error) {
	ctl, err := os.OpenFile("/dev/loop-control", os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer ctl.Close()
	// Another process can take the free device between asking and binding:
	// then ask again.
	for attempt := 0; ; attempt++ {
		n, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			return nil, fmt.Errorf("find a free loop device: %w", err)
		}
		dev := fmt.Sprintf("/dev/loop%d", n)
		if err := ensureNode(dev, n); err != nil {
			return nil, err
		}
		lf, err := os.OpenFile(dev, os.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		err = bindLoop(int(lf.Fd()), f)
		if err == nil {
			return lf, nil
		}
		lf.Close()
		if !errors.Is(err, unix.EBUSY) || attempt >= 20 {
			return nil, fmt.Errorf("bind %s: %w", dev, err)
		}
	}
}

func bindLoop(loopFd int, f *os.File) error {
	// Direct I/O: without it every block is cached twice, by the filesystem in
	// the image and again as the image file's own pages, and both copies are
	// charged to the writer's memory cgroup; a workspace near its memory limit
	// writing a big file then crawls.
	cfg := unix.LoopConfig{Fd: uint32(f.Fd()), Size: 4096}
	cfg.Info.Flags = unix.LO_FLAGS_AUTOCLEAR | unix.LO_FLAGS_DIRECT_IO
	copy(cfg.Info.File_name[:len(cfg.Info.File_name)-1], f.Name())
	err := unix.IoctlLoopConfigure(loopFd, &cfg)
	if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTTY) {
		return err
	}
	// kernels before 5.8 have no LOOP_CONFIGURE
	if err := unix.IoctlSetInt(loopFd, unix.LOOP_SET_FD, int(f.Fd())); err != nil {
		return err
	}
	if err := unix.IoctlLoopSetStatus64(loopFd, &cfg.Info); err != nil {
		_ = unix.IoctlSetInt(loopFd, unix.LOOP_CLR_FD, 0)
		return err
	}
	return nil
}

// ensureNode creates /dev/loopN if it is missing: a container's /dev is a
// snapshot taken when it started, so devices the kernel adds later (the host
// grows the loop device pool on demand) are not in it.
func ensureNode(dev string, n int) error {
	if _, err := os.Stat(dev); err == nil {
		return nil
	}
	major, minor := uint32(7), uint32(n)
	// the kernel's own numbering, which differs if the loop module has partitions
	if b, err := os.ReadFile(fmt.Sprintf("/sys/devices/virtual/block/loop%d/dev", n)); err == nil {
		if a, c, ok := strings.Cut(strings.TrimSpace(string(b)), ":"); ok {
			if ma, err1 := strconv.ParseUint(a, 10, 32); err1 == nil {
				if mi, err2 := strconv.ParseUint(c, 10, 32); err2 == nil {
					major, minor = uint32(ma), uint32(mi)
				}
			}
		}
	}
	err := unix.Mknod(dev, unix.S_IFBLK|0o660, int(unix.Mkdev(major, minor)))
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("create %s: %w", dev, err)
	}
	return nil
}

// Unmount unmounts dir if something is mounted there. A filesystem still busy
// (a sandbox's file server exiting) is retried for a few seconds, then detached.
func Unmount(dir string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		mounted, err := Mounted(dir)
		if err != nil || !mounted {
			return err
		}
		err = unix.Unmount(dir, 0)
		switch {
		case err == nil, errors.Is(err, unix.EINVAL): // EINVAL: gone meanwhile
		case errors.Is(err, unix.EBUSY) && time.Now().Before(deadline):
			time.Sleep(50 * time.Millisecond)
		case errors.Is(err, unix.EBUSY):
			return unix.Unmount(dir, unix.MNT_DETACH)
		default:
			return fmt.Errorf("unmount %s: %w", dir, err)
		}
	}
}

// Mounted reports whether a filesystem is mounted exactly at dir.
func Mounted(dir string) (bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && unescape(fields[4]) == abs {
			return true, nil
		}
	}
	return false, sc.Err()
}

// unescape decodes mountinfo's octal escapes (\040 for a space, and so on).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
