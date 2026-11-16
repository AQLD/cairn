"""Pulling a folder merges it the way the Go client and `cairn pull` do."""

import io
import os
import pathlib
import shutil
import stat
import sys
import tarfile
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
from cairn._client import _extract  # noqa: E402


def archive(*entries):
    """A tar of (name, kind, extra, mode): kind file (extra = bytes), dir,
    sym (extra = target) or link (extra = target)."""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w", format=tarfile.PAX_FORMAT) as tf:
        for name, kind, extra, mode in entries:
            ti = tarfile.TarInfo(name)
            ti.mode, ti.mtime = mode, 1_600_000_000
            if kind == "file":
                ti.size = len(extra)
                tf.addfile(ti, io.BytesIO(extra))
                continue
            ti.type = {"dir": tarfile.DIRTYPE, "sym": tarfile.SYMTYPE, "link": tarfile.LNKTYPE}[kind]
            if kind in ("sym", "link"):
                ti.linkname = extra
            tf.addfile(ti)
    buf.seek(0)
    return tarfile.open(fileobj=buf, mode="r:")


class TestExtract(unittest.TestCase):
    def setUp(self):
        self.dir = pathlib.Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.dir)
        self.dest = self.dir / "dest"
        self.dest.mkdir()
        self.outside = self.dir / "outside"
        self.outside.mkdir()

    def pull(self, *entries):
        return _extract(archive(*entries), str(self.dest))

    def test_modes_and_times(self):
        n = self.pull(("./", "dir", None, 0o755), ("shared.sh", "file", b"x", 0o777),
                      ("suid", "file", b"y", 0o4755), ("ro", "dir", None, 0o555), ("ro/f", "file", b"z", 0o444))
        self.addCleanup(os.chmod, self.dest / "ro", 0o755)  # so the folder can be removed
        self.assertEqual(n, 4)
        # group and other write bits are kept, as the Go client keeps them
        self.assertEqual(stat.S_IMODE(os.stat(self.dest / "shared.sh").st_mode), 0o777)
        # setuid is not
        self.assertEqual(stat.S_IMODE(os.stat(self.dest / "suid").st_mode), 0o755)
        # a read-only folder still got its file, then its own mode
        self.assertEqual((self.dest / "ro" / "f").read_bytes(), b"z")
        self.assertEqual(stat.S_IMODE(os.stat(self.dest / "ro").st_mode), 0o555)
        self.assertEqual(int(os.stat(self.dest / "shared.sh").st_mtime), 1_600_000_000)

    def test_a_link_is_replaced_not_written_through(self):
        (self.dest / "real.txt").write_text("keep me")
        os.symlink("real.txt", self.dest / "alias")
        self.pull(("alias", "file", b"new", 0o644))
        self.assertFalse(os.path.islink(self.dest / "alias"))
        self.assertEqual((self.dest / "alias").read_bytes(), b"new")
        self.assertEqual((self.dest / "real.txt").read_text(), "keep me")
        # and a file is replaced by a link
        self.pull(("real.txt", "sym", "alias", 0o777))
        self.assertEqual(os.readlink(self.dest / "real.txt"), "alias")

    def test_folders_merge_and_are_never_replaced_by_files(self):
        (self.dest / "d").mkdir()
        (self.dest / "d" / "mine.txt").write_text("local")
        (self.dest / "was-file").write_text("x")
        self.pull(("d", "dir", None, 0o755), ("d/theirs.txt", "file", b"remote", 0o644), ("was-file", "dir", None, 0o755))
        self.assertEqual(sorted(os.listdir(self.dest / "d")), ["mine.txt", "theirs.txt"])
        self.assertTrue((self.dest / "was-file").is_dir())
        with self.assertRaises(ValueError):
            self.pull(("d", "file", b"would drop the folder", 0o644))
        self.assertTrue((self.dest / "d" / "mine.txt").exists())

    def test_hard_links(self):
        self.pull(("a.txt", "file", b"same", 0o644), ("b.txt", "link", "a.txt", 0o644))
        self.assertEqual(os.stat(self.dest / "a.txt").st_ino, os.stat(self.dest / "b.txt").st_ino)

    def test_nothing_lands_outside(self):
        for entries in [
            [("../escape", "file", b"x", 0o644)],
            [("/abs", "file", b"x", 0o644)],
            [("a/../../escape", "file", b"x", 0o644)],
            # a link pointing out, then a file through it
            [("out", "sym", str(self.outside), 0o777), ("out/escape", "file", b"x", 0o644)],
            [("rel", "sym", "../outside", 0o777), ("rel/escape", "file", b"x", 0o644)],
            # a hard link to something outside
            [("h", "link", "../outside/secret", 0o644)],
            # a link pointing out, then a hard link through it
            [("out2", "sym", str(self.outside), 0o777), ("h2", "link", "out2/secret", 0o644)],
        ]:
            (self.outside / "secret").write_text("s")
            with self.subTest(entries=entries[-1][0]), self.assertRaises(ValueError):
                self.pull(*entries)
            self.assertEqual(sorted(os.listdir(self.outside)), ["secret"])
            self.assertFalse((self.dir / "escape").exists())

    def test_links_inside_are_followed(self):
        self.pull(("real", "dir", None, 0o755), ("alias", "sym", "real", 0o777), ("alias/f", "file", b"in", 0o644))
        self.assertEqual((self.dest / "real" / "f").read_bytes(), b"in")


if __name__ == "__main__":
    unittest.main()
