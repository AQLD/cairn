//go:build !linux

package disk

func Supported() error                      { return ErrUnsupported }
func Create(img string, sizeMB int64) error { return ErrUnsupported }
func Mount(img, dir string) error           { return ErrUnsupported }
func Unmount(dir string) error              { return nil }
func Mounted(dir string) (bool, error)      { return false, nil }
func Damaged(dir string) (bool, error)      { return false, nil }
func Repair(img string) error               { return ErrUnsupported }
