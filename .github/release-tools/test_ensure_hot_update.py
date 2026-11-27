import hashlib
from pathlib import Path
import unittest
from unittest import mock

import ensure_hot_update as updater


class PublishedAssetsTest(unittest.TestCase):
    def test_interrupted_upload_and_idempotent_verification(self):
        tag = 'v0.2.25-yoshub.66'
        archive = 'sub2api_0.2.25-yoshub.66_linux_amd64.tar.gz'
        files = {}
        uploads = []
        fail_checksum_once = True

        def assets(_tag):
            return {n: {'name': n, 'digest': 'sha256:' + hashlib.sha256(content).hexdigest()} for n, content in files.items()}

        def fake_gh(*args):
            nonlocal fail_checksum_once
            if args[0] == 'api':
                return 'a' * 40
            if args[1] == 'upload':
                path = Path(args[-1])
                if path.name == 'checksums.txt' and fail_checksum_once:
                    fail_checksum_once = False
                    raise OSError('simulated checksum upload interruption')
                self.assertNotIn(path.name, files)
                files[path.name] = path.read_bytes()
                uploads.append(path.name)
            else:
                self.assertEqual(args[1], 'download')
                name = args[args.index('-p') + 1]
                (Path(args[-1]) / name).write_bytes(files[name])

        def package(_image, folder, *, expected_version, expected_revision):
            self.assertEqual(expected_version, tag[1:])
            self.assertEqual(expected_revision, 'a' * 40)
            folder.mkdir()
            (folder / archive).write_bytes(b'fixture')

        with mock.patch.object(updater, 'gh', fake_gh), mock.patch.object(updater, 'release_assets', assets), mock.patch.object(updater, 'package', package):
            with self.assertRaises(OSError):
                updater.ensure(tag, 'local-image', False)
            updater.ensure(tag, 'local-image', False)
            updater.ensure(tag, 'local-image', True)
            self.assertEqual([archive, 'checksums.txt'], uploads)
            files[archive] = b'other binary'
            with self.assertRaisesRegex(ValueError, 'checksum differs'):
                updater.ensure(tag, None, True)
            del files['checksums.txt']
            with self.assertRaisesRegex(ValueError, 'unverified archive'):
                updater.ensure(tag, 'local-image', False)

    def test_checksum_must_be_unique_and_for_exact_platform(self):
        digest = 'a' * 64
        self.assertEqual(updater.checksum_for(f'{digest}  archive.tar.gz\n', 'archive.tar.gz'), digest)
        for text in (f'{digest}  archiveXtarXgz', f'{digest}  archive.tar.gz\n' * 2):
            with self.assertRaises(ValueError):
                updater.checksum_for(text, 'archive.tar.gz')
