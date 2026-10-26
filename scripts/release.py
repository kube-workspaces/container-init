#!/usr/bin/env python3
"""Version resolution, archive/checksum contract and standalone release notes.

Only the Python standard library is required. Publishing is deliberately owned
by the tag-only GitHub Actions job; these helpers never create tags or releases.
"""

import argparse
import gzip
import hashlib
import io
from pathlib import Path
import re
import struct
import subprocess
import tarfile


ROOT = Path(__file__).resolve().parent.parent
TARGETS = {"linux-amd64": 62, "linux-arm64": 183}  # ELF e_machine
RELEASE_TAG = re.compile(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\Z")
BUILD_VERSION = re.compile(r"[A-Za-z0-9][A-Za-z0-9.+_-]*\Z")


def checked_version(version):
    if not BUILD_VERSION.fullmatch(version) or len(version) > 128:
        raise ValueError("version must be a filename-safe Git version (at most 128 characters)")
    return version


def git(*args, cwd=None):
    return subprocess.check_output(
        ["git", *args], cwd=cwd or ROOT, text=True, stderr=subprocess.DEVNULL
    ).strip()


def resolve_version(ref="", cwd=None):
    # Tags take precedence over describe: multiple tags may point at the same
    # commit, and a release must carry exactly the tag that triggered its build.
    if ref.startswith("refs/tags/"):
        tag = ref.removeprefix("refs/tags/")
        if not RELEASE_TAG.fullmatch(tag):
            raise ValueError("release tag must be exact semver vMAJOR.MINOR.PATCH")
        if git("rev-parse", "HEAD", cwd=cwd) != git("rev-list", "-n", "1", tag, cwd=cwd):
            raise ValueError("release tag does not point at the checked-out commit")
        return checked_version(tag)
    try:
        return checked_version(git("describe", "--tags", "--always", "--dirty", cwd=cwd))
    except (FileNotFoundError, subprocess.CalledProcessError):
        return "dev"


def archive_name(version, target):
    return f"container-init-{checked_version(version)}-{target}.tar.gz"


def check_elf(data, target):
    """Reject wrong-target or dynamically linked binaries before publication."""
    if len(data) < 64 or data[:6] != b"\x7fELF\x02\x01":
        raise ValueError(f"{target}: binary must be little-endian ELF64")
    if struct.unpack_from("<H", data, 18)[0] != TARGETS[target]:
        raise ValueError(f"{target}: incorrect ELF architecture")
    offset = struct.unpack_from("<Q", data, 32)[0]
    size, count = struct.unpack_from("<HH", data, 54)
    if not count or size < 56 or offset + size * count > len(data):
        raise ValueError(f"{target}: invalid ELF program headers")
    for index in range(count):
        if struct.unpack_from("<I", data, offset + size * index)[0] == 3:  # PT_INTERP
            raise ValueError(f"{target}: binary is dynamically linked; build with CGO_ENABLED=0")


def payload(root, binary, version):
    return {
        "container-init": (binary, 0o755),
        "LICENSE": ((root / "LICENSE").read_bytes(), 0o644),
        "README.md": ((root / "README.md").read_bytes(), 0o644),
        "VERSION": ((version + "\n").encode(), 0o644),
        "examples/basic.yaml": ((root / "examples/basic.yaml").read_bytes(), 0o644),
        "examples/packages.yaml": ((root / "examples/packages.yaml").read_bytes(), 0o644),
    }


def package(bin_dir, dist_dir, version, root=ROOT):
    checked_version(version)
    # Check every source before creating archives, including the second target.
    binaries = {}
    for target in TARGETS:
        binary = (bin_dir / target / "container-init").read_bytes()
        check_elf(binary, target)
        binaries[target] = payload(root, binary, version)
    dist_dir.mkdir(parents=True, exist_ok=True)
    for target, files in binaries.items():
        name = archive_name(version, target)
        # Fixed tar metadata and gzip mtime make packaging the same inputs
        # repeatable. Do not glob old archives into the checksum manifest.
        with (dist_dir / name).open("wb") as output:
            with gzip.GzipFile(filename="", fileobj=output, mode="wb", mtime=0) as compressed:
                with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
                    base = f"container-init-{target}"
                    for directory in (base, base + "/examples"):
                        info = tarfile.TarInfo(directory)
                        info.type, info.mode = tarfile.DIRTYPE, 0o755
                        archive.addfile(info)
                    for path, (data, mode) in sorted(files.items()):
                        info = tarfile.TarInfo(f"{base}/{path}")
                        info.size, info.mode = len(data), mode
                        archive.addfile(info, io.BytesIO(data))
    checksums = []
    for target in TARGETS:
        name = archive_name(version, target)
        checksums.append(f"{hashlib.sha256((dist_dir / name).read_bytes()).hexdigest()}  {name}\n")
    (dist_dir / "SHA256SUMS").write_text("".join(checksums), encoding="utf-8")
    verify(dist_dir, version)


def verify(dist_dir, version):
    checked_version(version)
    expected = {archive_name(version, target) for target in TARGETS}
    lines = (dist_dir / "SHA256SUMS").read_text(encoding="utf-8").splitlines()
    seen = set()
    for line in lines:
        digest, separator, name = line.partition("  ")
        if not separator or not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise ValueError("invalid SHA256SUMS entry")
        if name not in expected or name in seen:
            raise ValueError("unexpected or duplicate SHA256SUMS filename")
        seen.add(name)
        if hashlib.sha256((dist_dir / name).read_bytes()).hexdigest() != digest:
            raise ValueError(f"checksum mismatch: {name}")
    if seen != expected:
        raise ValueError("SHA256SUMS must cover exactly both target archives")
    for target in TARGETS:
        with tarfile.open(dist_dir / archive_name(version, target), "r:gz") as archive:
            base = f"container-init-{target}"
            required = {
                base, base + "/examples", base + "/container-init", base + "/LICENSE",
                base + "/README.md", base + "/VERSION", base + "/examples/basic.yaml",
                base + "/examples/packages.yaml",
            }
            members = archive.getmembers()
            if len(members) != len(required) or {m.name for m in members} != required:
                raise ValueError(f"{target}: unexpected archive layout")
            for member in members:
                directory = member.name in (base, base + "/examples")
                if (directory and not member.isdir()) or (not directory and not member.isfile()):
                    raise ValueError(f"{target}: links/special files are not allowed")
                expected_mode = 0o755 if directory or member.name.endswith("/container-init") else 0o644
                if member.mode != expected_mode:
                    raise ValueError(f"{target}: unexpected archive permissions")
            if archive.extractfile(base + "/VERSION").read() != (version + "\n").encode():
                raise ValueError(f"{target}: archive VERSION differs from requested version")
            check_elf(archive.extractfile(base + "/container-init").read(), target)


def notes(version, repository):
    """Classify direct commits as well as PR merges, as in the org's releases.

    The deploy generator's nonfunctional preamble currently assumes chart-aligned
    component versions except for desktop-client. Keep this standalone binary's
    preamble local rather than incorrectly claiming Helm appVersion alignment.
    .github/release.yml retains the shared automatic-release-note categories.
    """
    if not RELEASE_TAG.fullmatch(version):
        raise ValueError("release notes require exact semver vMAJOR.MINOR.PATCH")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("repository must be owner/name")
    try:
        tags = git("tag", "--merged", version + "^", "--sort=-version:refname").splitlines()
    except subprocess.CalledProcessError:
        tags = []
    previous = next((tag for tag in tags if RELEASE_TAG.fullmatch(tag)), None)
    revision = f"{previous}..{version}" if previous else version
    commits = git("log", "--no-merges", "--format=%H%x00%s", revision).splitlines()
    categories = {name: [] for name in (
        "⚠️ Breaking changes", "🔒 Security", "✨ Features", "🐛 Fixes",
        "📚 Documentation", "🔧 CI and tooling", "⬆️ Dependencies", "Other changes",
    )}
    for commit in commits:
        sha, _, subject = commit.partition("\0")
        if re.match(r"[a-z]+(?:\([^)]*\))?!:", subject):
            category = "⚠️ Breaking changes"
        elif re.match(r"(?:security|fix\(security\)):", subject):
            category = "🔒 Security"
        elif re.match(r"(?:build|chore)\(deps[^)]*\):", subject):
            category = "⬆️ Dependencies"
        else:
            prefix = subject.split(":", 1)[0].split("(", 1)[0]
            category = {
                "feat": "✨ Features", "fix": "🐛 Fixes", "docs": "📚 Documentation",
                "security": "🔒 Security",
                "ci": "🔧 CI and tooling", "build": "🔧 CI and tooling",
                "chore": "🔧 CI and tooling", "style": "🔧 CI and tooling",
                "test": "🔧 CI and tooling", "deps": "⬆️ Dependencies",
            }.get(prefix, "Other changes")
        # Commit titles can contain Markdown; avoid turning them into HTML.
        subject = subject.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
        categories[category].append(f"- {subject} ([`{sha[:7]}`](https://github.com/{repository}/commit/{sha}))")
    base = f"https://github.com/{repository}/releases/download/{version}"
    text = [
        "Cloud-config initialization for Linux containers (`container-init`).",
        "", "## Download", "", "| Platform | Architecture | Archive |", "|---|---|---|",
    ]
    for target, arch in (("linux-amd64", "x86_64"), ("linux-arm64", "arm64")):
        name = archive_name(version, target)
        text.append(f"| Linux | {arch} | [`{name}`]({base}/{name}) |")
    text += [
        "", f"Both archives are covered by [`SHA256SUMS`]({base}/SHA256SUMS).",
        "", "## Install", "", "Download both archives and SHA256SUMS, then verify:",
        "", "```sh", "sha256sum -c SHA256SUMS",
        f"tar -xzf {archive_name(version, 'linux-amd64')}",
        "./container-init-linux-amd64/container-init --version", "```", "",
        "Use the arm64 archive on arm64 containers. Each archive includes the binary,",
        "README, license, VERSION and example cloud-config documents.", "",
        "Initialization runs inside the main container; an installer init container",
        "can distribute the binary but does not share the application's root filesystem.", "",
        f"See the [versioned README](https://github.com/{repository}/blob/{version}/README.md)",
        "for the supported cloud-config subset, execution behavior and integration status.",
        "", "## What's Changed", "",
    ]
    if previous:
        text += [f"{len(commits)} direct commit(s) since `{previous}`.", ""]
    else:
        text += ["First release: there is no previous release tag to compare against.", ""]
    for category, entries in categories.items():
        if entries:
            text += [f"### {category}", "", *entries, ""]
    if previous:
        text += [f"**Full changelog:** https://github.com/{repository}/compare/{previous}...{version}", ""]
    return "\n".join(text)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    version = commands.add_parser("version", help="resolve the Git build or exact release-tag version")
    version.add_argument("--ref", default="", help="GitHub ref, supplied explicitly by CI")
    for command in ("package", "verify"):
        sub = commands.add_parser(command)
        sub.add_argument("--dist-dir", type=Path, default=Path("dist"))
        sub.add_argument("--version", required=True)
        if command == "package":
            sub.add_argument("--bin-dir", type=Path, default=Path("bin"))
    release_notes = commands.add_parser("notes")
    release_notes.add_argument("--version", required=True)
    release_notes.add_argument("--repo", default="kube-workspaces/container-init")
    args = parser.parse_args()
    try:
        if args.command == "version":
            print(resolve_version(args.ref))
        elif args.command == "package":
            package(args.bin_dir, args.dist_dir, args.version)
            print(f"Packaged and verified {args.version}: two archives and SHA256SUMS")
        elif args.command == "verify":
            verify(args.dist_dir, args.version)
            print(f"Verified {args.version}: layout, permissions, static ELF targets, versions and checksums")
        elif args.command == "notes":
            print(notes(args.version, args.repo))
    except (ValueError, OSError, tarfile.TarError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"release: {error}\n")


if __name__ == "__main__":
    main()
