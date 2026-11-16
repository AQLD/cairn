"""The client finds the engine `cairn up` started through its config file."""

import json
import os
import pathlib
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
import cairn  # noqa: E402


class TestConfig(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.path = os.path.join(self.dir.name, "config.json")
        env = {k: v for k, v in os.environ.items() if not k.startswith("CAIRN_")}
        env["CAIRN_CONFIG"] = self.path
        patcher = mock.patch.dict(os.environ, env, clear=True)
        patcher.start()
        self.addCleanup(patcher.stop)

    def write(self, cfg):
        with open(self.path, "w", encoding="utf-8") as f:
            json.dump(cfg, f)

    def test_no_config(self):
        c = cairn.Client()
        self.assertEqual((c.endpoint, c.token), ("http://127.0.0.1:7420", ""))

    def test_config_file(self):
        self.write({"endpoint": "http://127.0.0.1:7431", "token": "file"})
        c = cairn.Client()
        self.assertEqual((c.endpoint, c.token), ("http://127.0.0.1:7431", "file"))

    def test_env_endpoint_never_gets_file_token(self):
        self.write({"endpoint": "http://127.0.0.1:7431", "token": "file"})
        os.environ["CAIRN_ENDPOINT"] = "http://elsewhere:7420"
        c = cairn.Client()
        self.assertEqual((c.endpoint, c.token), ("http://elsewhere:7420", ""))
        c = cairn.Client("http://other:7420")
        self.assertEqual(c.token, "")

    def test_env_token_wins(self):
        self.write({"endpoint": "http://127.0.0.1:7431", "token": "file"})
        os.environ["CAIRN_TOKEN"] = "env"
        self.assertEqual(cairn.Client().token, "env")

    def test_broken_config_is_ignored(self):
        with open(self.path, "w", encoding="utf-8") as f:
            f.write("{not json")
        self.assertEqual(cairn.Client().endpoint, "http://127.0.0.1:7420")


if __name__ == "__main__":
    unittest.main()
