# Compatibility

Cairn workspaces run on [gVisor](https://gvisor.dev), which implements the Linux system-call interface in its own kernel instead of passing calls to the host. Almost everything an agent runs works unchanged. This page lists the differences we know of, what Cairn does about each, and what your code should do.

Tested on gVisor release 20261005 (the minimum Cairn supports), systrap platform, Ubuntu 24.04 hosts.

## Known differences

### Renaming out of `/tmp` fails with `EXDEV`

`/tmp` in a workspace is its own mount: Cairn binds the workspace's own `/tmp` folder there so that its files are kept by saves (runsc would otherwise mount an empty tmpfs). Because it is a separate mount, `rename()` from `/tmp` to anywhere else fails with `EXDEV` ("Invalid cross-device link"), as it would between two disks.

Tools that write a temporary file in `/tmp` and then rename it into place elsewhere hit this. Most libraries fall back to copy-and-delete on `EXDEV` (`shutil.move`, `mv`); `os.rename` and `os.replace` in Python, and `fs.rename` in Node, do not.

**What to do:** create temporary files next to their destination (`tempfile.NamedTemporaryFile(dir=dest_dir)`), or use a move that handles `EXDEV`.

### Connections to the outside do not survive a move

A full save keeps processes and their connections inside the workspace (loopback TCP, Unix sockets). Connections to hosts outside the workspace are not: they can fail when a full save is resumed, and always do when it resumes on another server or is copied (a copy has a new IP address, and the remote side has moved on).

A program in the middle of a download or holding a database connection sees the connection fail after resume, and has to reconnect.

**What to do:** retry network operations, as you would for any flaky network. pip, for one, already does: in our tests it retried a download interrupted by a move and finished it.

### Full saves restore only with the gVisor release that made them

gVisor's checkpoint format is internal to each release, so a full save (memory and processes) made with one release does not restore with another. Quick saves (files only) are not affected.

Cairn records the release in every full save and restores with that same release. Releases are installed side by side under `/opt/gvisor/<release>/`, and `RUNSC_KEEP` in [deploy/versions.env](../deploy/versions.env) lists the older releases a host keeps. When upgrading gVisor, move the previous `RUNSC_RELEASE` into `RUNSC_KEEP`; removing a release strands the full saves made with it (and paused workspaces, which are full saves).

Releases 20260914, 20260921 and 20260928 restore checkpoints but leave sleeping processes frozen. 20261005 is the minimum.

### Copies keep their processes' random generators, not the OS's randomness

A full save holds processes' memory, and a random generator seeded inside a process (Python's `random`, NumPy, a game engine's RNG) is part of it. So the source and every copy of the save continue the same sequence. That's what an experiment needs when it replays identical branches.

The operating system's randomness (`/dev/urandom`, `getrandom`, so new keys, UUIDs and TLS) is fresh in every copy, so copies never share it.

To make copies independent, reseed after the copy: `random.seed(os.urandom(16))`. (Scenarios G2.6, G2.8, G17.4.)

### Lock-heavy multithreaded Python is slower

Programs whose threads contend on locks many thousands of times a second (CPython threads sharing the GIL are the common case) run about 4x slower than under runc, because each contended wait is a system call that gVisor handles itself. Single-threaded code, multiprocessing, I/O-bound threads and short commands are not affected; short commands start faster than in a Docker container.

**What to do:** prefer `multiprocessing` or `concurrent.futures.ProcessPoolExecutor` for CPU-bound parallel Python.

## What works

Tested under gVisor through Cairn: git, pip and venv, npm, gcc, Go, `make -j`, SQLite in WAL mode, Python multiprocessing and shared memory, Node worker threads, `/proc`, Unix sockets, HTTPS, archives, ripgrep, ssh-keygen, `fs.watch` and inotify, IPv6, large fsync-heavy writes, signals and threads. Headless Chromium, a Node dev server, a Python process with open files and a package install in progress all resume from a full save with the same processes.

Found something that behaves differently in a workspace? Open an issue with the command and what you expected.
