// Package integration tests the built static executable in disposable Linux
// containers. It is opt-in because package tests need Docker and network access.
package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
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
			t.Run("unsupported-option-prevents-all-mutations", func(t *testing.T) {
				run(t, "#cloud-config\nbootcmd: [[touch, /run/must-not-exist]]\nchpasswd: {list: unsupported}\n", 0,
					"/bin/sh", "-c", `/tools/container-init --user-data -; status=$?; test "$status" -ne 0 && test ! -e /run/must-not-exist`)
			})
			t.Run("accounts-trust-timezone-and-privilege-drop", func(t *testing.T) {
				cert, key := identityFixtures(t)
				data := fmt.Sprintf(`#cloud-config
groups: [developers, {observers: [root]}]
users:
  - name: developer
    uid: 1234
    primary_group: developers
    groups: [observers]
    homedir: /home/developer
    shell: /bin/sh
    lock_passwd: false
    ssh_authorized_keys: [%q]
  - name: locked
    uid: 1235
    lock_passwd: true
chpasswd:
  expire: false
  users:
    - {name: developer, password: disposable-test-password, type: text}
    - {name: locked, password: disposable-test-password, type: text}
ssh_authorized_keys: [%q]
ca_certs:
  trusted:
    - |
      %s
timezone: Etc/UTC
write_files:
  - path: /home/developer/initialized
    content: ready
    owner: developer:developers
    defer: true
runcmd:
  - [test, -s, /home/developer/initialized]
  - [sh, -ec, 'test "$(stat -c %%a /home/developer/.ssh)" = 700; test "$(stat -c %%a /home/developer/.ssh/authorized_keys)" = 600; test "$(wc -l < /home/developer/.ssh/authorized_keys)" -eq 1']
  - [sh, -ec, 'openssl verify -CAfile /etc/ssl/certs/ca-certificates.crt /usr/local/share/ca-certificates/container-init-*.crt']
  - [sh, -ec, 'cmp /etc/localtime /usr/share/zoneinfo/Etc/UTC; test "$(cat /etc/timezone)" = Etc/UTC']
`, key, key, strings.ReplaceAll(strings.TrimSpace(cert), "\n", "\n      "))
				run(t, data, 0, "/bin/sh", "-ec", `if command -v apk >/dev/null; then apk add --no-cache shadow ca-certificates tzdata openssl; else apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y passwd ca-certificates tzdata openssl; fi
cat > /run/full-config
/tools/container-init --user-data /run/full-config --default-user developer --status-file /run/provision-status.json
/tools/container-init --user-data /run/full-config --default-user developer --status-file /run/provision-status.json
test "$(stat -c %a /run/provision-status.json)" = 600
awk -F: '$1=="developer" {found=1; if ($2 !~ /^\$/) exit 1} END {if (!found) exit 1}' /etc/shadow
awk -F: '$1=="locked" {found=1; if ($2 !~ /^!/) exit 1} END {if (!found) exit 1}' /etc/shadow
hash=$(openssl passwd -6 -salt integration disposable-test-password)
printf '#cloud-config\nchpasswd:\n  users:\n    - name: developer\n      password: %s\n' "$hash" > /run/hash-config
/tools/container-init --user-data /run/hash-config
awk -F: -v want="$hash" '$1=="developer" {found=1; if ($2 != want || $3 != 0) exit 1} END {if (!found) exit 1}' /etc/shadow
exec /tools/container-init --user-data /run/full-config --default-user developer --run-as developer -- /bin/sh -exc 'test "$(id -u)" = 1234; test "$HOME" = /home/developer; test "$USER" = developer; test -r /home/developer/initialized; test ! -r /etc/shadow; for g in $(id -G); do test "$g" != 0; done; grep -q "NoNewPrivs:.*1" /proc/self/status'`)
			})
		})
	}
}

func identityFixtures(t *testing.T) (string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "container-init integration CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	blob := binary.BigEndian.AppendUint32(nil, uint32(len("ssh-ed25519")))
	blob = append(blob, []byte("ssh-ed25519")...)
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(pub)))
	blob = append(blob, pub...)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " integration"
}
