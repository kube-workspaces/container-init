package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func dockerTest(t *testing.T, input string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker operation failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func integrationBin(t *testing.T) string {
	t.Helper()
	if os.Getenv("CONTAINER_INIT_INTEGRATION") != "1" {
		t.Skip("run make test-integration")
	}
	bin, err := filepath.Abs("../../bin")
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestShellFreeTargetAndNumericOwnership(t *testing.T) {
	bin := integrationBin(t)
	image := "container-init-shell-free:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	dockerTest(t, "FROM scratch\nCOPY container-init /container-init\nENTRYPOINT [\"/container-init\"]\n", "build", "-q", "-t", image, "-f", "-", bin)
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", image).Run() })
	out := dockerTest(t, "#cloud-config\nwrite_files:\n  - path: /run/next-config\n    content: \"#cloud-config\\nruncmd: []\\n\"\n    owner: '0:0'\n", "run", "--rm", "-i", image, "--user-data", "-", "--", "/container-init", "--user-data", "/run/next-config", "--validate")
	if !strings.Contains(out, "cloud-config is valid") {
		t.Fatal("shell-free handoff did not observe initialized file")
	}
}

func TestApplicationSignalHandoff(t *testing.T) {
	bin := integrationBin(t)
	for _, image := range []string{"debian:trixie-slim", "alpine:3.22"} {
		t.Run(image, func(t *testing.T) {
			name := "container-init-signal-" + strconv.FormatInt(time.Now().UnixNano(), 10)
			t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
			dockerTest(t, "", "run", "-d", "--name", name, "--mount", "type=bind,src="+bin+",dst=/tools,readonly", image, "sh", "-ec", `printf '#cloud-config\nruncmd: []\n' > /run/config; exec /tools/container-init --user-data /run/config -- sh -c 'trap "exit 42" TERM; touch /run/app-ready; while :; do sleep 0.1; done'`)
			deadline := time.Now().Add(10 * time.Second)
			for exec.Command("docker", "exec", name, "test", "-f", "/run/app-ready").Run() != nil {
				if time.Now().After(deadline) {
					t.Fatal("application did not start")
				}
				time.Sleep(100 * time.Millisecond)
			}
			dockerTest(t, "", "kill", "--signal=TERM", name)
			if code := dockerTest(t, "", "wait", name); code != "42" {
				t.Fatalf("application did not receive SIGTERM or preserve exit code: %s", code)
			}
		})
	}
}
