// Package integration tests the built static executable in disposable Linux
// containers. It is opt-in because package tests need Docker and network access.
package integration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContainerIntegration(t *testing.T) {
	if os.Getenv("CONTAINER_INIT_INTEGRATION") != "1" {
		t.Skip("run make test-integration to enable disposable Docker tests")
	}
	bin, err := filepath.Abs("../../bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bin, "container-init")); err != nil {
		t.Fatal("build bin/container-init before running integration tests")
	}
	for _, image := range []string{"debian:trixie-slim", "alpine:3.22"} {
		t.Run(image, func(t *testing.T) {
			t.Parallel()
			run := func(t *testing.T, data string, wantExit int, command ...string) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				args := []string{"run", "--rm", "-i", "--mount", "type=bind,src=" + bin + ",dst=/tools,readonly", image}
				args = append(args, command...)
				cmd := exec.CommandContext(ctx, "docker", args...)
				cmd.Stdin = strings.NewReader(data)
				output, err := cmd.CombinedOutput()
				exitCode := 0
				if err != nil {
					var exit *exec.ExitError
					if !errors.As(err, &exit) {
						t.Fatalf("docker: %v\n%s", err, output)
					}
					exitCode = exit.ExitCode()
				}
				if exitCode != wantExit {
					t.Fatalf("expected exit %d, got %d: %v\n%s", wantExit, exitCode, err, output)
				}
			}
			t.Run("packages", func(t *testing.T) {
				run(t, "#cloud-config\npackages: [curl, ca-certificates]\nruncmd: [[curl, --version]]\n", 0,
					"/tools/container-init", "--user-data", "-", "--", "/bin/sh", "-c", "test -x /usr/bin/curl")
			})
			t.Run("files-argv-and-exit-status", func(t *testing.T) {
				run(t, `#cloud-config
bootcmd:
  - [test, '!', -e, /run/container-init-test/early]
write_files:
  - path: /run/container-init-test/early
    content: 'ready'
    permissions: '0640'
  - path: /run/container-init-test/deferred
    encoding: base64
    content: ZGVmZXJyZWQ=
    defer: true
runcmd:
  - [test, -s, /run/container-init-test/early]
  - [test, -s, /run/container-init-test/deferred]
`, 7, "/tools/container-init", "--user-data", "-", "--", "/bin/sh", "-c",
					`test -s /run/container-init-test/early && test "$1" = 'a b' && test "$2" = '' && test "$3" = '$HOME' && exit 7`,
					"app", "a b", "", "$HOME")
			})
			t.Run("pending-key-prevents-all-mutations", func(t *testing.T) {
				run(t, "#cloud-config\nbootcmd: [[touch, /run/must-not-exist]]\nchpasswd: {}\n", 0,
					"/bin/sh", "-c", `/tools/container-init --user-data -; status=$?; test "$status" -ne 0 && test ! -e /run/must-not-exist`)
			})
		})
	}
}
