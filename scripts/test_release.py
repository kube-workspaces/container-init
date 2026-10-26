"""Exercise artifact/version contracts without changing real Git tags or releases."""

import hashlib
import io
import os
from pathlib import Path
import struct
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import release


def elf(machine, program_type=1):
    data = bytearray(120)
    data[:6] = b"\x7fELF\x02\x01"
    struct.pack_into("<H", data, 18, machine)
    struct.pack_into("<Q", data, 32, 64)
    struct.pack_into("<HH", data, 54, 56, 1)
    struct.pack_into("<I", data, 64, program_type)
    return bytes(data)


class ArchiveTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / "bin"
        self.dist = self.root / "dist"
        for target, machine in release.TARGETS.items():
            directory = self.bin / target
            directory.mkdir(parents=True)
            (directory / "container-init").write_bytes(elf(machine))
        (self.root / "examples").mkdir()
        for name in ("LICENSE", "README.md", "examples/basic.yaml", "examples/packages.yaml"):
            (self.root / name).write_text(f"fixture: {name}\n", encoding="utf-8")
        self.version = "v1.2.3"

    def package(self):
        release.package(self.bin, self.dist, self.version, root=self.root)

    def rewrite_checksum(self, name):
        lines = (self.dist / "SHA256SUMS").read_text().splitlines()
        digest = hashlib.sha256((self.dist / name).read_bytes()).hexdigest()
        lines = [f"{digest}  {name}" if line.endswith("  " + name) else line for line in lines]
        (self.dist / "SHA256SUMS").write_text("\n".join(lines) + "\n")

    def test_package_layout_modes_versions_and_repeatability(self):
        self.package()
        first = (self.dist / "SHA256SUMS").read_bytes()
        self.package()
        self.assertEqual((self.dist / "SHA256SUMS").read_bytes(), first)
        for target in release.TARGETS:
            with tarfile.open(self.dist / release.archive_name(self.version, target)) as archive:
                binary = archive.getmember(f"container-init-{target}/container-init")
                self.assertEqual(binary.mode, 0o755)
                self.assertEqual(binary.uid, 0)
                self.assertEqual(binary.mtime, 0)
                version = archive.extractfile(f"container-init-{target}/VERSION").read()
                self.assertEqual(version, b"v1.2.3\n")

    def test_missing_second_target_fails_before_packaging(self):
        (self.bin / "linux-arm64/container-init").unlink()
        with self.assertRaises(FileNotFoundError):
            self.package()
        self.assertFalse(self.dist.exists())

    def test_wrong_architecture_or_dynamic_linker_rejected(self):
        for data in (elf(62), elf(183, program_type=3), b"not ELF"):
            with self.subTest(data=data[:20]):
                (self.bin / "linux-arm64/container-init").write_bytes(data)
                with self.assertRaises(ValueError):
                    self.package()
                self.assertFalse(self.dist.exists())

    def test_stale_archives_not_included_in_manifest(self):
        self.dist.mkdir()
        (self.dist / "stale.tar.gz").write_bytes(b"stale")
        self.package()
        self.assertNotIn("stale", (self.dist / "SHA256SUMS").read_text())
        release.verify(self.dist, self.version)

    def test_checksum_tampering_missing_and_duplicate_entries_rejected(self):
        self.package()
        original = (self.dist / "SHA256SUMS").read_text()
        first = original.splitlines()[0]
        for manifest in (first + "\n", original + first + "\n", "0" * 64 + original[64:]):
            with self.subTest(manifest=manifest):
                (self.dist / "SHA256SUMS").write_text(manifest)
                with self.assertRaises(ValueError):
                    release.verify(self.dist, self.version)
        (self.dist / "SHA256SUMS").write_text(original)
        name = release.archive_name(self.version, "linux-amd64")
        (self.dist / name).write_bytes(b"tampered archive")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            release.verify(self.dist, self.version)

    def test_archive_contract_verified_independently_of_checksum(self):
        for change in ("version", "mode", "symlink", "unexpected", "architecture"):
            with self.subTest(change=change):
                self.package()
                name = release.archive_name(self.version, "linux-amd64")
                members = []
                with tarfile.open(self.dist / name) as archive:
                    for member in archive.getmembers():
                        data = archive.extractfile(member).read() if member.isfile() else None
                        if member.name.endswith("/VERSION") and change == "version":
                            data = b"wrong-version\n"
                            member.size = len(data)
                        if member.name.endswith("/container-init"):
                            if change == "mode":
                                member.mode = 0o644
                            elif change == "symlink":
                                member.type = tarfile.SYMTYPE
                                member.linkname = "/other-binary"
                                member.size, data = 0, None
                            elif change == "architecture":
                                data = elf(183)
                        if member.name.endswith("/README.md") and change == "unexpected":
                            member.name = "../../README.md"
                        members.append((member, data))
                with tarfile.open(self.dist / name, "w:gz") as archive:
                    for member, data in members:
                        archive.addfile(member, io.BytesIO(data) if data is not None else None)
                self.rewrite_checksum(name)
                with self.assertRaises(ValueError):
                    release.verify(self.dist, self.version)

    def test_unsafe_versions_rejected(self):
        for version in ("../escape", "bad\nversion", "", "-option", "a" * 129):
            with self.subTest(version=version):
                with self.assertRaises(ValueError):
                    release.package(self.bin, self.dist, version, root=self.root)


class GitVersionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.env = {
            **os.environ,
            "GIT_AUTHOR_NAME": "Release fixture", "GIT_COMMITTER_NAME": "Release fixture",
            "GIT_AUTHOR_EMAIL": "release@example.invalid",
            "GIT_COMMITTER_EMAIL": "release@example.invalid",
        }
        self.git("init", "--quiet")
        self.commit("feat: initial runner")

    def git(self, *args):
        return subprocess.check_output(
            ["git", *args], cwd=self.root, env=self.env,
            stderr=subprocess.DEVNULL, text=True,
        ).strip()

    def commit(self, subject):
        path = self.root / "source.txt"
        path.write_text(path.read_text() + subject + "\n" if path.exists() else subject + "\n")
        self.git("add", "source.txt")
        self.git("commit", "--quiet", "-m", subject)

    def test_tag_version_wins_when_multiple_tags_point_to_same_commit(self):
        self.git("tag", "v1.2.3")
        self.git("tag", "v2.0.0")
        self.assertEqual(release.resolve_version("refs/tags/v1.2.3", cwd=self.root), "v1.2.3")

    def test_exact_release_tags_and_checked_out_commit_required(self):
        for tag in ("v01.2.3", "v1.2", "v1.2.3-hotfix", "v1.2.3-rc.1", "1.2.3"):
            self.git("tag", tag)
            with self.subTest(tag=tag):
                with self.assertRaises(ValueError):
                    release.resolve_version("refs/tags/" + tag, cwd=self.root)
        self.git("tag", "v1.2.3")
        self.commit("fix: next commit")
        with self.assertRaisesRegex(ValueError, "checked-out commit"):
            release.resolve_version("refs/tags/v1.2.3", cwd=self.root)

    def test_development_versions_and_dirty_worktree(self):
        self.git("tag", "v1.2.3")
        self.commit("fix: command handling")
        self.assertRegex(release.resolve_version(cwd=self.root), r"^v1\.2\.3-1-g[0-9a-f]+$")
        (self.root / "source.txt").write_text("local edits\n")
        self.assertTrue(release.resolve_version(cwd=self.root).endswith("-dirty"))

    def test_source_export_falls_back_to_dev(self):
        with tempfile.TemporaryDirectory() as source_export:
            self.assertEqual(release.resolve_version(cwd=source_export), "dev")
        with patch.object(release, "git", side_effect=FileNotFoundError):
            self.assertEqual(release.resolve_version(), "dev")

    def test_first_release_and_direct_commit_notes(self):
        self.git("tag", "v0.1.0")
        with patch.object(release, "ROOT", self.root):
            first = release.notes("v0.1.0", "kube-workspaces/container-init")
            self.assertIn("First release", first)
            self.assertIn("feat: initial runner", first)
            self.assertIn("container-init-v0.1.0-linux-arm64.tar.gz", first)
        self.commit("fix: preserve argv")
        self.commit("ci: verify archives")
        self.git("tag", "v0.1.1")
        with patch.object(release, "ROOT", self.root):
            followup = release.notes("v0.1.1", "kube-workspaces/container-init")
        self.assertIn("### 🐛 Fixes", followup)
        self.assertIn("### 🔧 CI and tooling", followup)
        self.assertIn("v0.1.0...v0.1.1", followup)
        self.assertNotIn("initial runner", followup)
        self.assertNotIn("Helm", followup)


if __name__ == "__main__":
    unittest.main()
