#!/usr/bin/env python3
"""Build the two YosHub hot-update assets from an already-built local Linux image."""
import argparse
import hashlib
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile


def package(image, output):
    identity = subprocess.check_output(
        ['docker', 'image', 'inspect', '--format', '{{.Id}} {{.Os}} {{.Architecture}}', image], text=True
    ).strip()
    image_id, os_name, arch = identity.split()
    if os_name != 'linux' or arch not in {'amd64', 'arm64'}:
        raise ValueError(f'unsupported image platform: {os_name}/{arch}')
    run = ['docker', 'run', '--rm', '--pull=never', '--network=none', '--read-only', '--cap-drop=ALL']
    result = subprocess.check_output(run + ['--entrypoint', '/app/sub2api', image_id, '--version'],
                                     stderr=subprocess.STDOUT, text=True)
    match = re.search(r'\bSub2API (\d+\.\d+\.\d+-yoshub\.\d+) \(commit:', result)
    if not match:
        raise ValueError('image must contain a numbered YosHub release binary')
    name = f'sub2api_{match[1]}_{os_name}_{arch}.tar.gz'
    output.mkdir(exist_ok=True, parents=True)
    # Never overwrite a previous package or another architecture's checksum list.
    if any((output / file).exists() for file in (name, 'checksums.txt')):
        raise FileExistsError('use a new output directory for each target')
    with tempfile.TemporaryDirectory(dir=output) as tmp:
        work = Path(tmp)
        binary = work / 'sub2api'
        with binary.open('wb') as stream:
            subprocess.run(run + ['--entrypoint', '/bin/cat', image_id, '/app/sub2api'], stdout=stream, check=True)
        with tarfile.open(work / name, 'w:gz', compresslevel=3) as archive, binary.open('rb') as stream:
            info = tarfile.TarInfo('sub2api')
            info.mode, info.size = 0o755, binary.stat().st_size
            archive.addfile(info, stream)
        with (work / name).open('rb') as stream:
            digest = hashlib.file_digest(stream, 'sha256').hexdigest()
        (work / 'checksums.txt').write_bytes(f'{digest}  {name}\n'.encode('ascii'))
        (work / name).rename(output / name)
        (work / 'checksums.txt').rename(output / 'checksums.txt')
    print(f'{name}\n{digest}')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('image', help='local image tag or ID')
    parser.add_argument('output', type=Path, help='output directory for this version and architecture')
    args = parser.parse_args()
    package(args.image, args.output)
