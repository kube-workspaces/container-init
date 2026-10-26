# container-init

A Go executable for initializing Linux containers from an explicit subset of
cloud-init `#cloud-config`, before launching their application. Intended for
both kube-workspaces `container` and `scratch` workspaces, and usable standalone.

**Status:** initial executable foundation. Six of the selected twelve top-level
keys are implemented. Opt-in platform integration requires matching Image/
Workspace CRDs, API and controller. Account/SSH/CA/timezone adapters, privilege
dropping and persistent execution state are pending. Compatibility is
with the documented subset, not every cloud-init module or nested option.

## Build and run

Requires Go 1.24 or newer and Linux. The executable builds without CGO.
Make targets that resolve a Git version or package releases also require Python 3.9+.

```sh
make build
bin/container-init --list-keys
bin/container-init --version
bin/container-init --user-data examples/basic.yaml --validate

# Run as root inside your container; provisioning only:
bin/container-init --user-data examples/basic.yaml --dry-run
bin/container-init --user-data examples/basic.yaml

# Provision, then replace this process with an explicit application argv:
bin/container-init --user-data /run/container-init/user-data -- /usr/local/bin/app --flag

# stdin is also supported:
bin/container-init --user-data - --validate < examples/basic.yaml
```

`--validate` parses and validates the entire supported document without checking
host tools or changing files. `--dry-run` also preflights package-manager/root,
shell and early-file ownership requirements, then lists step labels. Neither
executes user commands or launches an application. Validation does not guarantee
that runtime commands, network access, package versions or file writes succeed.
Executables created by package installation and deferred file owners are
resolved at their execution phase, not required to exist during preflight.

## Initial twelve-key scope

Based on the [cloud-init module reference](https://docs.cloud-init.io/en/latest/reference/modules.html)
reviewed on 2026-10-26. Twelve configuration keys cover nine upstream modules.

| Key | Current implementation |
|---|---|
| `bootcmd` | Early shell strings or nonempty argv lists. |
| `write_files` | Inline text, `!!binary`, base64 and gzip content; owner/group; octal permissions; append; deferred writes; create absent parent directories. |
| `groups` | Planned; rejected before provisioning. |
| `users` | Planned; rejected before provisioning. |
| `chpasswd` | Planned; rejected before provisioning. |
| `ssh_authorized_keys` | Planned; rejected before provisioning. |
| `ca_certs` | Planned; rejected before provisioning. |
| `timezone` | Planned; rejected before provisioning. |
| `package_update` | Boolean cache refresh with APT/APK. |
| `package_upgrade` | Boolean upgrade with APT/APK, after cache refresh. |
| `packages` | Names and `[name, version]` pairs with APT/APK; cache refresh is automatic for installation. |
| `runcmd` | Late shell strings or verbatim argv lists; null entries skipped. |

### Configuration details

- One YAML document, beginning with `#cloud-config`, with a mapping at its root.
  Unknown keys/options, duplicate mapping keys, YAML aliases/anchors/merges,
  pending keys and invalid types fail before provisioning. Errors omit scalar
  payloads. Input is bounded to 4 MiB and each decoded file to 16 MiB.
- Commands are ordered as supplied. Shell strings use `/bin/sh -c`; argv arrays
  do not interpret shell variables or quoting. Arguments must be YAML strings
  (quote numbers, booleans, colons, and version strings where necessary).
  Each entry runs in a separate process: shell variables and working-directory
  changes do not persist to later entries. Use one multiline shell string when
  commands must share shell state. This differs from cloud-init's generated
  `runcmd` script and is part of the initial compatibility subset.
- Files require an absolute `path`. Omitted content creates an empty file.
  Default ownership is `root:root`, default mode `0644`. `owner` requires an
  explicit `user:group`, with names or numeric IDs. Non-root users must explicitly
  select writable paths and their own UID/GID. Modes must be strings between
  `0000` and `0777`; setuid/setgid/sticky file modes are not yet supported.
  Newly created parents use `0755` subject to umask and receive the file owner;
  existing parent directories retain their ownership and permissions.
- File encodings: `text/plain`, `b64`, `base64`, `gz`, `gzip`, `gz+base64`,
  `gzip+base64`, `gz+b64`, `gzip+b64`. Gzip binary content can use YAML
  `!!binary`. Remote `source` content is not yet implemented and is rejected.
- Package adapters select Debian/Ubuntu `apt-get` or Alpine `apk` from the
  image's `/etc/os-release` `ID`. Root and the corresponding executable must
  already exist. APT runs with `DEBIAN_FRONTEND=noninteractive`. Version pairs
  become native `name=version` arguments. Manager-keyed objects, snap, other
  distro IDs, deprecated `apt_*` aliases and automatic reboot are not supported.

## Execution contract

Order: full document validation and available-host preflight → `bootcmd` → early
`write_files` → package cache refresh → optional upgrade → installs → deferred
`write_files` → `runcmd` → optional application exec.

Every invocation runs all requested steps. There are no once-per-instance
markers or resumable phases yet. Avoid assuming that arbitrary commands are
idempotent or have exactly-once effects. A PVC marker surviving replacement does
not mean the replacement container retains packages installed in the old root
filesystem. Upstream cloud-init normally runs many modules once per instance;
this executable intentionally has a simpler initial lifecycle.

The first failure stops provisioning and prevents application launch. Completed
steps are not rolled back. SIGINT/SIGTERM cancels provisioning and kills the
active command's process group. The final application uses `exec`, retaining
stdio, environment, PID and its own exit status and receiving signals directly.
This is not a general PID 1 supervisor/reaper; applications that require one
should use their normal init/supervisor in the explicit application argv.
Background daemons launched during provisioning are not supervised.

Phase logs contain step labels, not configuration content or command arguments.
Command stdout/stderr are streamed to the container's stdout/stderr: user
commands and external tools can themselves print sensitive data. Execution
uses the invoking identity and is not sandboxed. Privilege dropping is pending;
if initialization runs as root, the application also runs as root unless its
explicit entrypoint drops privileges itself.

## Baked-in and injected usage

Include the binary in an image and invoke it from an entrypoint:

```sh
#!/bin/sh
set -e
/usr/local/bin/container-init --user-data /run/container-init/user-data
exec /usr/local/bin/original-entrypoint "$@"
```

Alternatively, an installer init container can copy the static binary into a
shared `emptyDir`. Mount that volume read-only in the main container and make
the binary its entrypoint, passing the explicit application command after `--`.
The architecture of the copied binary must match the application container.

**Provisioning must execute inside the main container** for packages/accounts
to affect its root filesystem. Running the tool only in an ordinary init
container can prepare shared volumes, but cannot modify the main container's
separate writable root. Static Go linking does not provide missing shells,
package managers or account tools.

Kubernetes does not automatically prepend an executable to an OCI entrypoint.
Injected usage requires explicit application argv; no registry entrypoint
discovery is performed. In kube-workspaces, `Image.spec.defaultContainerInit`
opts in, `defaultCommand`/`defaultArgs` supply startup, and `defaultUserData`
provides the default document. The API snapshots these settings; the controller
owns Secret delivery and PodSpec injection. Omitted/disabled initialization
preserves ordinary startup. Baked-in mode only delivers the data file.
See the [platform integration guide](https://github.com/kube-workspaces/deploy/blob/main/docs/container-init.md).

### Installer OCI image

The Dockerfile packages the already-built static executable with Alpine's copy
utility. It uses no cross-architecture RUN steps, so multi-architecture packaging
does not require QEMU. Locally:

```sh
make build-all VERSION=local
docker build -t container-init:local .
```

The Build workflow publishes `ghcr.io/kube-workspaces/container-init:sha-<full-commit>`
and `ghcr.io/kube-workspaces/container-init:latest` on main pushes; exact release
tag pushes also publish `:<version>`. PR/manual builds validate the image without
pushing. Publishing is gated on native builds/tests and lint and consumes the
verified per-target binary artifacts. The tag release waits for image publication
as well as archive packaging.

Configure a published immutable reference through Helm's
`controller.containerInitImage` or the controller's `KW_CONTAINER_INIT_IMAGE` /
`--container-init-image` setting. Empty disables injected mode. The controller
uses the image's `/usr/local/bin/container-init` and `/bin/sh` to copy the runner
into its shared writable installer mount; the main container mounts it read-only.
Image pull access/visibility must be configured like other workspace images.

## Development

```sh
make check
make lint
make build

# Requires Docker and network access; installs packages only in --rm containers:
make test-integration
```

Unit tests run commands and file writes only in temporary test locations;
package adapter tests use a fake runner and never install packages on the host.
Opt-in integration tests cover real APT/APK installation in disposable Debian
and Alpine containers, file visibility, explicit argv/exit-status handoff and
rejection of pending configuration before any commands execute.

`make lint` uses golangci-lint v2.13.2, matching CI. `make check` also runs the
Python release-helper tests. Workflows are checked in CI with actionlint v1.7.12.

## Versioned builds and release artifacts

The version follows the same convention as the desktop client and workspace
agent: `git describe --tags --always --dirty`, falling back to `dev` in a source
export. Builds from an exact release tag use that tag. CI resolves the triggering
tag explicitly so another tag on the same commit cannot change the release
version. There is no manually maintained source version to bump.

`make build`, `make build-all` and `make run` pass `-X main.version=$(VERSION)`
through Go linker flags. `container-init --version` prints that exact string.
Direct `go build`/`go run` without linker flags report `dev`. A `VERSION` override
is useful for source exports and reproducible packaging inputs:

```sh
make version
make build VERSION=v0.1.0
bin/container-init --version
make run VERSION=v0.1.0 ARGS=--version

# Cross-build both static Linux targets, then package and verify:
make build-all package verify-release VERSION=v0.1.0
```

`make package` consumes `bin/linux-amd64/container-init` and
`bin/linux-arm64/container-init`. It creates:

```text
dist/
  container-init-<version>-linux-amd64.tar.gz
  container-init-<version>-linux-arm64.tar.gz
  SHA256SUMS
```

Each tarball has one stable root directory, independent of the version:

```text
container-init-linux-<arch>/
  container-init
  VERSION
  README.md
  LICENSE
  examples/
    basic.yaml
    packages.yaml
```

The binary is executable (`0755`), documentation/configuration files are `0644`,
and archive metadata is normalized for repeatable packaging. The verifier checks
the exact layout, permissions, VERSION, target ELF architecture, absence of a
dynamic interpreter and checksums for both archives. Native CI jobs additionally
execute each architecture's binary and check its reported version before it is
packaged. The assembled amd64 archive is extracted and smoke-tested again.

### CI and publishing

The **Build** workflow runs on pushes to `main`, pull requests targeting `main`,
manual dispatches and pushes of `v*` tags. It calls the reusable **CI** and
**Lint** workflows so tagged releases are gated by the same checks as main/PRs:

- Native Linux amd64/arm64 vet/race tests, release-helper tests and version smoke
  tests; disposable Debian/Alpine integration tests on amd64.
- Formatting, golangci-lint and actionlint checks.
- Native static binaries, per-target archive artifacts and a `SHA256SUMS`
  artifact available from the workflow run, including development/PR builds.

Only a **tag push** with exact semver `vMAJOR.MINOR.PATCH` publishes a GitHub
Release. Tags with prerelease/hotfix suffixes or missing components are rejected;
use a deliberate patch-version bump for fixes, consistent with workspace-agent.
Manual dispatch builds artifacts without publishing a release, even on a tag.
Tags and version bumps are never created automatically.

When deliberately cutting a release after committing/pushing the desired code:

```sh
git tag -a v0.1.0 -m 'container-init v0.1.0'
git push origin v0.1.0
```

The release job consumes the already-built archives, verifies them again,
generates download instructions and categorized notes from direct commits,
and publishes both tarballs plus `SHA256SUMS`. A rerun updates the existing
release and replaces those assets. The automatic-release-note configuration
is identical to the other kube-workspaces projects. The local notes helper uses
the same category structure while describing this as a standalone binary,
rather than using the shared deploy script's chart-alignment preamble.

To verify a downloaded release, place both archives and `SHA256SUMS` in one
directory and run `sha256sum -c SHA256SUMS`. Extract the matching architecture
and run `./container-init-linux-<arch>/container-init --version`.

The cross-repository implementation plan is maintained in the kube-workspaces
tracking repository as `container-init-implementation-plan.md`.
