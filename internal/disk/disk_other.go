//go:build !linux

package disk

func Supported() error                                  { return ErrUnsupported }
func Create(img string, sizeMB int64) error             { return ErrUnsupported }
func Mount(img, dir string) error                       { return ErrUnsupported }
func Unmount(dir string) error                          { return nil }
func Mounted(dir string) (bool, error)                  { return false, nil }
func Damaged(dir string) (bool, error)                  { return false, nil }
func Repair(img string) error                           { return ErrUnsupported }
func MountOverlay(lower, upper, work, dir string) error { return ErrUnsupported }
func OverlaySupported(scratch string) error             { return ErrUnsupported }
func OverlayReportsWriteErrors(scratch string) error    { return ErrUnsupported }
func Freeze(dir string) error                           { return nil }
func Thaw(dir string) error                             { return nil }
