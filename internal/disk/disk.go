// Package disk gives a workspace a filesystem of fixed size: a sparse ext4
// image file, loop-mounted where the workspace's files live.
//
// Writing past the size fails with ENOSPC ("No space left on device") inside
// the workspace, like a full disk, and never touches the space other
// workspaces or the engine need. The image is sparse and mounted with discard,
// so it takes host space only for what is actually stored: an empty 10 GiB
// image uses about 4 MB, and deleting a file inside gives its space back.
//
// Images need Linux, root, mkfs.ext4 and loop devices; Supported says whether
// this machine has them.
package disk

import "errors"

// MinSizeMB is the smallest image: below this ext4 spends most of it on itself.
const MinSizeMB = 32

// ErrUnsupported is returned where images cannot be used.
var ErrUnsupported = errors.New("disk images are not supported here")
