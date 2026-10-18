#!/usr/bin/env python3
"""Split GoReleaser builds across runners without requiring GoReleaser Pro."""
import argparse
import hashlib
import itertools
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import zipfile
from datetime import datetime, timezone

import yaml

FULL_CONFIG = Path('.goreleaser.yaml')
SIMPLE_CONFIG = Path('.goreleaser.simple.yaml')
VERSION_FILE = Path('backend/cmd/server/VERSION')
VERSION_RE = re.compile(r'\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?')
MAINTENANCE_RE = re.compile(r'(\d+\.\d+\.\d+)-ablett\.([1-9][0-9]*)(?:-(.+))?')
SOURCE = 'ablettx/sub2api'
ROLLBACK_FLOOR = '0.2.17-ablett.1'


def maintenance_parts(version):
    match = MAINTENANCE_RE.fullmatch(version)
    if not match:
        raise ValueError('Fork release requires X.Y.Z-ablett.N version')
    return match.group(1), int(match.group(2)), match.group(3) is None


def migration_checksums():
    return {p.name: hashlib.sha256(p.read_text().strip().encode()).hexdigest()
            for p in sorted(Path('backend/migrations').glob('*.sql')) if p.read_text().strip()}


def binary_sha256(archive, target):
    name = 'sub2api.exe' if target['goos'] == 'windows' else 'sub2api'
    if target['goos'] == 'windows':
        with zipfile.ZipFile(archive) as zf:
            entries = [entry for entry in zf.infolist() if entry.filename in (name, './' + name)]
            if len(entries) != 1 or entries[0].is_dir():
                raise ValueError('archive requires one regular binary')
            return hashlib.sha256(zf.read(entries[0])).hexdigest()
    with tarfile.open(archive, 'r:gz') as tf:
        entries = [entry for entry in tf.getmembers() if entry.name in (name, './' + name)]
        if len(entries) != 1 or not entries[0].isfile():
            raise ValueError('archive requires one regular binary')
        with tf.extractfile(entries[0]) as stream:
            return hashlib.file_digest(stream, 'sha256').hexdigest()


def manifest_metadata(version, sha, target, name, digest, binary_digest):
    base, revision, stable = maintenance_parts(version)
    if not re.fullmatch('[a-f0-9]{40}', sha):
        raise ValueError('invalid source commit')
    return {'schema_version': 1, 'source': SOURCE, 'channel': 'stable' if stable else 'preview',
            'version': version, 'upstream_version': base, 'revision': revision, 'sha': sha,
            'target': target, 'archive': name, 'sha256': digest, 'binary_sha256': binary_digest,
            'capabilities': {'account_user_quota': 1}, 'database_min': 1, 'database_max': 1,
            'rollback_floor': ROLLBACK_FLOOR, 'migrations': migration_checksums()}


def config(simple=False):
    return yaml.safe_load((SIMPLE_CONFIG if simple else FULL_CONFIG).read_text())


def targets(simple=False):
    build = config()['builds'][0]
    result = []
    for goos, goarch in itertools.product(build['goos'], build['goarch']):
        item = {'goos': goos, 'goarch': goarch}
        if any(all(item.get(k) == v for k, v in rule.items()) for rule in build.get('ignore', [])):
            continue
        if not simple or item == {'goos': 'linux', 'goarch': 'amd64'}:
            result.append(item)
    if not result:
        raise ValueError('empty release target matrix')
    return result


def archive_name(version, target):
    if not VERSION_RE.fullmatch(version) or target not in targets():
        raise ValueError('invalid release version or target')
    suffix = 'zip' if target['goos'] == 'windows' else 'tar.gz'
    return f"sub2api_{version}_{target['goos']}_{target['goarch']}.{suffix}"


def sha256(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def plan(args):
    sha = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
    if args.dry_run:
        version = VERSION_FILE.read_text().strip()
        tag = 'v' + version
    else:
        tag = args.ref
        version = tag.removeprefix('v')
        if not tag.startswith('v') or not VERSION_RE.fullmatch(version):
            raise ValueError('publishing requires a v-prefixed release version tag')
        tagged_sha = subprocess.check_output(['git', 'rev-parse', '--verify', f'refs/tags/{tag}^{{commit}}'], text=True).strip()
        if sha != tagged_sha:
            raise ValueError('checkout does not match the selected release tag')
        if subprocess.check_output(['git', 'status', '--porcelain', '--untracked-files=normal'], text=True).strip():
            raise ValueError('publishing requires a clean source checkout')
    if not VERSION_RE.fullmatch(version):
        raise ValueError('invalid VERSION')
    maintenance_parts(version)
    if os.environ.get('GITHUB_REPOSITORY', SOURCE) != SOURCE:
        raise ValueError('release source must be the configured Fork')
    if not Path('backend/migrations/246_ablett_account_user_quota.sql').is_file():
        raise ValueError('selected source does not contain the quota capability')
    VERSION_FILE.write_text(version + '\n')
    result = {'sha': sha, 'tag': tag, 'version': version,
              'owner_lower': os.environ.get('GITHUB_REPOSITORY_OWNER', '').lower(),
              'simple': str(args.simple).lower(), 'dry_run': str(args.dry_run).lower(),
              'date': datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'),
              'matrix': json.dumps({'include': targets(args.simple)}, separators=(',', ':'))}
    with Path(os.environ['GITHUB_OUTPUT']).open('a') as output:
        for key, value in result.items():
            output.write(f'{key}={value}\n')


def generate_config(args):
    data = config(args.simple if args.mode == 'publish' else False)
    version = os.environ.get('RELEASE_VERSION', VERSION_FILE.read_text().strip())
    _, _, stable = maintenance_parts(version)
    data['release']['prerelease'] = not stable
    data['release']['make_latest'] = 'true' if stable and not args.simple else 'false'
    data['snapshot'] = {'version_template': '{{ .Env.RELEASE_VERSION }}'}
    data['dockers'] = []
    data['docker_manifests'] = []
    if args.mode == 'build':
        target = {'goos': args.goos, 'goarch': args.goarch}
        if target not in targets():
            raise ValueError('unsupported build target')
        for build in data['builds']:
            build['goos'], build['goarch'], build['ignore'] = [args.goos], [args.goarch], []
            build['ldflags'] = [re.sub(r'{{\s*\.Date\s*}}', '{{ .Env.RELEASE_DATE }}', flag)
                                for flag in build.get('ldflags', [])]
    else:
        # Artifacts are supplied through the OSS extra_files mechanism. No build
        # is repeated on the publishing runner, and release templates stay intact.
        data['before'] = {'hooks': []}
        data['builds'] = [{'id': 'sub2api', 'skip': True}]
        data['archives'] = []
        extra = [{'glob': 'release-input/sub2api_*.tar.gz'}, {'glob': 'release-input/sub2api_*.zip'},
                 {'glob': 'release-input/manifest-*.json'}]
        if args.simple:
            data['checksum'] = {'disable': True}
        else:
            data['release']['extra_files'] = extra
            data['checksum'] = {'name_template': 'checksums.txt', 'algorithm': 'sha256', 'extra_files': extra}
    Path(args.output).write_text(yaml.safe_dump(data, sort_keys=False, allow_unicode=True))


def collect(args):
    target = {'goos': args.goos, 'goarch': args.goarch}
    name = archive_name(args.version, target)
    source = Path('dist') / name
    checksums = {line.split()[1].lstrip('*'): line.split()[0] for line in Path('dist/checksums.txt').read_text().splitlines()}
    digest = sha256(source)
    if checksums.get(name) != digest:
        raise ValueError('archive does not match the build checksum')
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, output / name)
    manifest = manifest_metadata(args.version, args.sha, target, name, digest, binary_sha256(source, target))
    (output / f"manifest-{args.goos}-{args.goarch}.json").write_text(json.dumps(manifest) + '\n')


def verify(args):
    directory = Path(args.input)
    expected = set()
    for target in targets(args.simple):
        name = archive_name(args.version, target)
        manifest_name = f"manifest-{target['goos']}-{target['goarch']}.json"
        expected.update((name, manifest_name))
        manifest = json.loads((directory / manifest_name).read_text())
        if manifest.get('sha256') != sha256(directory / name):
            raise ValueError(f'archive checksum mismatch: {name}')
        if manifest != manifest_metadata(args.version, args.sha, target, name, sha256(directory / name),
                                         binary_sha256(directory / name, target)):
            raise ValueError(f'build provenance or checksum mismatch: {name}')
    if {p.name for p in directory.iterdir()} != expected:
        raise ValueError('missing or unexpected release artifacts')


def contexts(args):
    verify(args)
    for target in targets(args.simple):
        if target['goos'] != 'linux':
            continue
        dest = Path(args.output) / target['goarch']
        dest.mkdir(parents=True, exist_ok=True)
        with tarfile.open(Path(args.input) / archive_name(args.version, target), 'r:gz') as archive:
            members = [member for member in archive.getmembers() if member.name in ('sub2api', './sub2api')]
            if len(members) != 1 or not members[0].isfile():
                raise ValueError('archive must contain one regular sub2api binary')
            with archive.extractfile(members[0]) as source, (dest / 'sub2api').open('wb') as output:
                shutil.copyfileobj(source, output)
        (dest / 'sub2api').chmod(0o755)
        shutil.copy2('Dockerfile.goreleaser', dest / 'Dockerfile')
        (dest / 'deploy').mkdir(exist_ok=True)
        shutil.copy2('deploy/docker-entrypoint.sh', dest / 'deploy/docker-entrypoint.sh')
        shutil.copytree('backend/resources', dest / 'backend/resources', dirs_exist_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    p = commands.add_parser('plan')
    p.add_argument('--ref', required=True)
    p.add_argument('--simple', action='store_true')
    p.add_argument('--dry-run', action='store_true')
    p.set_defaults(run=plan)
    p = commands.add_parser('config')
    p.add_argument('mode', choices=['build', 'publish'])
    p.add_argument('--goos')
    p.add_argument('--goarch')
    p.add_argument('--simple', action='store_true')
    p.add_argument('--output', required=True)
    p.set_defaults(run=generate_config)
    p = commands.add_parser('collect')
    for arg in ('version', 'sha', 'goos', 'goarch', 'output'):
        p.add_argument('--' + arg, required=True)
    p.set_defaults(run=collect)
    for command, handler in [('verify', verify), ('contexts', contexts)]:
        p = commands.add_parser(command)
        for arg in ('version', 'sha', 'input'):
            p.add_argument('--' + arg, required=True)
        p.add_argument('--simple', action='store_true')
        if command == 'contexts':
            p.add_argument('--output', required=True)
        p.set_defaults(run=handler)
    args = parser.parse_args()
    args.run(args)


if __name__ == '__main__':
    main()
