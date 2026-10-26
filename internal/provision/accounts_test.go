package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kube-workspaces/container-init/internal/config"
)

func TestIdentityPreflightPreventsBootCommands(t *testing.T) {
	for _, body := range []string{
		"users: [default]", "groups: [{developers: [nonexistent_container_init_user]}]",
		"ssh_authorized_keys: []\nusers: [{name: test}]", "ca_certs: {trusted: []}\ntimezone: Missing/Zone",
	} {
		c, err := config.Parse(strings.NewReader("#cloud-config\nbootcmd: [[must-not-run]]\n" + body + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		r := &recorder{}
		o := testOptions(r, "alpine")
		if strings.Contains(body, "name: test") {
			o.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
		}
		if _, err = Build(c, o); err == nil {
			t.Fatalf("preflight accepted %s", body)
		}
		if len(r.calls) != 0 {
			t.Fatal("preflight executed a command")
		}
	}
}

func TestPasswordRunnerSuppressesAllToolOutput(t *testing.T) {
	var logs bytes.Buffer
	r := ProcessRunner{Stdout: &logs, Stderr: &logs}
	err := r.RunInput(context.Background(), []string{"/bin/sh", "-c", "cat; cat >&2; exit 7"}, strings.NewReader("do-not-print-me\n"))
	if err == nil || logs.Len() != 0 || strings.Contains(err.Error(), "do-not-print-me") {
		t.Fatalf("password runner disclosed input: %v %s", err, logs.String())
	}
}

type inputRecorder struct {
	recorder
	input string
}

func (r *inputRecorder) RunInput(_ context.Context, args []string, input io.Reader) error {
	data, err := io.ReadAll(input)
	r.input = string(data)
	r.calls = append(r.calls, invocation{args: args})
	return err
}

func TestPasswordTransportAndExpiry(t *testing.T) {
	for _, hashed := range []bool{false, true} {
		r := &inputRecorder{}
		o := testOptions(&r.recorder, "debian")
		o.Commands = r
		p, err := Build(&config.Config{Passwords: []config.Password{{Name: "root", Value: "do-not-print-me", Hashed: hashed}}, ExpirePasswords: true}, o)
		if err != nil {
			t.Fatal(err)
		}
		if err = p.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r.input != "root:do-not-print-me\n" || len(r.calls) != 2 || r.calls[1].args[0] != "chage" {
			t.Fatal("password transport/expiry failed")
		}
		for _, call := range r.calls {
			for _, arg := range call.args {
				if strings.Contains(arg, "do-not-print-me") {
					t.Fatal("password in argv")
				}
			}
		}
		if (len(r.calls[0].args) == 2) != hashed {
			t.Fatal("hash option not preserved")
		}
	}
}

func TestStatusRecordsFailureAndAlwaysReruns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "status.json")
	r := &recorder{hook: func([]string) error { return errors.New("do-not-print-me") }}
	o := testOptions(r, "debian")
	o.Status = StatusWriter(path, "hash-only")
	p, err := Build(&config.Config{RunCmd: []config.Command{{Args: []string{"fail"}}}}, o)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]interface{}
	if err = json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s["state"] != "failed" || s["step"] != "runcmd[0]" || s["configSHA256"] != "hash-only" || strings.Contains(string(data), "do-not-print-me") {
		t.Fatalf("invalid status: %s", data)
	}
	r.hook = nil
	for i := 0; i < 2; i++ {
		if err = p.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.calls) != 3 {
		t.Fatal("status incorrectly skipped previously completed steps")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("status permissions")
	}
}
