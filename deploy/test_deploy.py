import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
import zipfile

SPEC = importlib.util.spec_from_file_location("scf_deploy", Path(__file__).with_name("deploy.py"))
deploy = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(deploy)


class DeploymentTests(unittest.TestCase):
    def test_zip_contains_executable_bootstrap_and_binary(self):
        with tempfile.TemporaryDirectory() as temporary:
            binary = Path(temporary) / "linux-binary"
            data = bytearray(64)
            data[:5] = b"\x7fELF\x02"
            data[18:20] = (62).to_bytes(2, "little")
            binary.write_bytes(data)
            with zipfile.ZipFile(io.BytesIO(deploy.package_binary(binary))) as archive:
                self.assertEqual(set(archive.namelist()), {"scf-tunnel", "scf_bootstrap"})
                for name in archive.namelist():
                    self.assertTrue(archive.getinfo(name).external_attr >> 16 & 0o111)
                self.assertIn(b"exec ./scf-tunnel -node", archive.read("scf_bootstrap"))

    def test_rejects_wrong_platform_binary(self):
        with tempfile.TemporaryDirectory() as temporary:
            binary = Path(temporary) / "not-linux"
            binary.write_bytes(b"not an ELF binary")
            with self.assertRaises(ValueError):
                deploy.package_binary(binary)

    def test_private_file_permissions(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "nested" / "agent.json"
            deploy.private_write(path, '{"password":"test-only"}')
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            deploy.private_write(path, "replacement")
            self.assertEqual(path.read_text(), "replacement")
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    def test_agent_upgrade_preserves_auth_and_ledger(self):
        old = {"token": "unchanged-token", "password": "unchanged-password", "user": "unchanged-user",
               "ledger_path": "/private/existing-ledger.json", "monthly_limit_yuan": 8, "nodes": []}
        new = deploy.make_agent_config({}, [{"region": "test", "url": "wss://example.invalid/tunnel"}], "unchanged-token", old)
        for key in ("token", "password", "user", "ledger_path", "monthly_limit_yuan"):
            self.assertEqual(new[key], old[key])
        self.assertEqual(len(new["nodes"]), 1)
        self.assertEqual(old["nodes"], [])
        with self.assertRaises(ValueError):
            deploy.make_agent_config({}, [], "different-token", old)

    def test_example_settings_resolve_without_cloud_access(self):
        config, state, agent = deploy.load_settings(Path(__file__).parents[1] / "config/deploy.toml.example")
        self.assertEqual(len(config["deployment"]["regions"]), 5)
        self.assertTrue(state.is_absolute())
        self.assertTrue(agent.is_absolute())


if __name__ == "__main__":
    unittest.main()
