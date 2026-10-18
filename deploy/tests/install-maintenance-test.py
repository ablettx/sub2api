"""Verify the installer gate without downloading files or touching a service."""
import copy
import hashlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'install.sh'
SOURCE = SCRIPT.read_text().split('verify_maintenance_package() {', 1)[1].split("<<'PY'\n", 1)[1].split('\nPY\n}', 1)[0]


class MaintenanceInstallerTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.name = 'sub2api_0.2.17-ablett.2_linux_arm64.tar.gz'
        self.binary = b'isolated maintenance fixture'
        self.archive()
        self.manifest = {
            'schema_version': 1, 'source': 'ablettx/sub2api', 'channel': 'stable',
            'version': '0.2.17-ablett.2', 'upstream_version': '0.2.17', 'revision': 2,
            'target': {'goos': 'linux', 'goarch': 'arm64'}, 'archive': self.name,
            'capabilities': {'account_user_quota': 1}, 'database_min': 1, 'database_max': 1,
            'sha': 'a' * 40, 'sha256': hashlib.sha256((self.root / self.name).read_bytes()).hexdigest(),
            'binary_sha256': hashlib.sha256(self.binary).hexdigest(),
            'rollback_floor': '0.2.17-ablett.1', 'migrations': {'246_quota.sql': 'checksum'}
        }

    def archive(self, unsafe=False):
        with tarfile.open(self.root / self.name, 'w:gz') as archive:
            entry = tarfile.TarInfo('sub2api')
            entry.size = len(self.binary)
            archive.addfile(entry, io.BytesIO(self.binary))
            if unsafe:
                archive.addfile(tarfile.TarInfo('../escape'))

    def verify(self, manifest):
        (self.root / 'manifest.json').write_text(json.dumps(manifest))
        return subprocess.run([sys.executable, '-O', '-c', SOURCE, str(self.root),
                               '0.2.17-ablett.2', 'linux', 'arm64', self.name], capture_output=True)

    def test_valid_package(self):
        self.assertEqual(self.verify(self.manifest).returncode, 0)

    def test_invalid_metadata_is_rejected_even_with_python_optimization(self):
        for field, value in [('source', 'Wei-Shaw/sub2api'), ('capabilities', {}),
                             ('channel', 'preview'), ('database_min', 2), ('sha256', '0' * 64),
                             ('binary_sha256', '0' * 64), ('target', {'goos': 'linux', 'goarch': 'amd64'})]:
            with self.subTest(field=field):
                manifest = copy.deepcopy(self.manifest)
                manifest[field] = value
                self.assertNotEqual(self.verify(manifest).returncode, 0)

    def test_incompatible_existing_database_is_rejected(self):
        current = copy.deepcopy(self.manifest)
        current['migrations']['999_incompatible.sql'] = 'unexpected'
        (self.root / 'current.json').write_text(json.dumps(current))
        self.assertNotEqual(self.verify(self.manifest).returncode, 0)

    def test_archive_paths_are_checked(self):
        self.archive(unsafe=True)
        self.manifest['sha256'] = hashlib.sha256((self.root / self.name).read_bytes()).hexdigest()
        self.assertNotEqual(self.verify(self.manifest).returncode, 0)


if __name__ == '__main__':
    unittest.main()
