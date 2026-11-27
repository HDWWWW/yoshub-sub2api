#!/usr/bin/env python3
"""Complete or verify the amd64 hot update on a published YosHub release."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tempfile

from package_hot_update import package

REPO = 'HDWWWW/yoshub-sub2api'
REGISTRY = 'ghcr.io/hdwwww/yoshub-sub2api'


def gh(*args):
    return subprocess.check_output(['gh', *args], text=True, encoding='utf-8').strip()


def release_assets(tag):
    release = json.loads(gh('release', 'view', tag, '-R', REPO, '--json', 'tagName,isDraft,isPrerelease,assets'))
    if release['tagName'] != tag or release['isDraft'] or release['isPrerelease']:
        raise ValueError('expected a published stable release')
    return {asset['name']: asset for asset in release['assets']}


def checksum_for(text, name):
    matches = re.findall(r'^([0-9a-f]{64})[ \t]+\*?' + re.escape(name) + r'\r?$', text, re.M)
    if len(matches) != 1:
        raise ValueError(f'checksums.txt must contain exactly one SHA256 for {name}')
    return matches[0]


def asset_digest(tag, asset, folder):
    digest = asset.get('digest') or ''
    if re.fullmatch(r'sha256:[0-9a-f]{64}', digest):
        return digest.removeprefix('sha256:')
    # Some older GitHub assets have no API digest.
    folder.mkdir(parents=True, exist_ok=True)
    gh('release', 'download', tag, '-R', REPO, '-p', asset['name'], '-D', str(folder))
    with (folder / asset['name']).open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def ensure(tag, image, check_only):
    match = re.fullmatch(r'(?:image-)?v(\d+\.\d+\.\d+-yoshub\.\d+)', tag)
    if not match:
        raise ValueError('expected a numbered YosHub release tag')
    version = match[1]
    name = f'sub2api_{version}_linux_amd64.tar.gz'
    assets = release_assets(tag)
    with tempfile.TemporaryDirectory() as tmp:
        work = Path(tmp)
        checksums = work / 'checksums.txt'
        expected = None
        if 'checksums.txt' in assets:
            gh('release', 'download', tag, '-R', REPO, '-p', 'checksums.txt', '-D', tmp)
            expected = checksum_for(checksums.read_text(encoding='ascii'), name)
        if check_only and (name not in assets or expected is None):
            raise ValueError('missing archive or checksums.txt')
        if name not in assets or expected is None:
            revision = gh('api', f'repos/{REPO}/commits/{tag}', '--jq', '.sha')
            if not image:
                image = f'{REGISTRY}:v{version}'
                subprocess.run(['docker', 'pull', '--platform=linux/amd64', image], check=True)
            package(image, work / 'built', expected_version=version, expected_revision=revision)
            built = work / 'built' / name
            with built.open('rb') as stream:
                digest = hashlib.file_digest(stream, 'sha256').hexdigest()
            if expected is not None and digest != expected:
                raise ValueError('image archive conflicts with the existing checksum')
            if name in assets:
                if asset_digest(tag, assets[name], work / 'built-remote') != digest:
                    raise ValueError('existing unverified archive differs from tagged image')
            else:
                gh('release', 'upload', tag, '-R', REPO, str(built))
                assets = release_assets(tag)
        digest = asset_digest(tag, assets[name], work)
        if expected is None:
            if any(other != name and re.fullmatch(r'sub2api_[\w.-]+\.(?:tar\.gz|zip)', other) for other in assets):
                raise ValueError('restore the multi-platform checksum file from the original publisher')
            checksums.write_bytes(f'{digest}  {name}\n'.encode('ascii'))
            gh('release', 'upload', tag, '-R', REPO, str(checksums))
            verify_dir = work / 'published'
            verify_dir.mkdir()
            gh('release', 'download', tag, '-R', REPO, '-p', 'checksums.txt', '-D', str(verify_dir))
            expected = checksum_for((verify_dir / 'checksums.txt').read_text(encoding='ascii'), name)
        if expected != digest:
            raise ValueError('published checksum differs from archive SHA256')
    print(f'{tag} verified: {name} {digest}')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('tag')
    parser.add_argument('--image', help='use this already-loaded Linux amd64 image when packaging')
    parser.add_argument('--check-only', action='store_true')
    args = parser.parse_args()
    ensure(args.tag, args.image, args.check_only)
