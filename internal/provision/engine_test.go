package provision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kube-workspaces/container-init/internal/config"
)

type invocation struct {
	args []string
	env  []string
}

type recorder struct {
	calls []invocation
	hook  func([]string) error
}

func (r *recorder) Run(_ context.Context, args, env []string) error {
	r.calls = append(r.calls, invocation{args, env})
	if r.hook != nil {
		return r.hook(args)
	}
	return nil
}

func testOptions(r *recorder, distro string) Options {
	return Options{
		Commands: r,
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		DistroID: func() (string, error) { return distro, nil },
		EUID:     func() int { return 0 },
	}
}

func TestOrderedProvisioningAndFileVisibility(t *testing.T) {
	dir := t.TempDir()
	early := filepath.Join(dir, "new", "early")
	late := filepath.Join(dir, "late")
	owner := fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid())
	c := &config.Config{
		BootCmd: []config.Command{{Args: []string{"boot"}}},
		WriteFiles: []config.File{
			{Path: late, Content: []byte("late"), Owner: owner, Permissions: 0600, Defer: true},
			{Path: early, Content: []byte("early"), Owner: owner, Permissions: 0640},
		},
		Packages: []config.Package{{Name: "example", Version: "1.2"}},
		RunCmd:   []config.Command{{Args: []string{"late-command", "arg with spaces", ""}}},
	}
	r := &recorder{hook: func(args []string) error {
		switch args[0] {
		case "boot":
			if _, err := os.Stat(early); !os.IsNotExist(err) {
				t.Fatal("early file written before bootcmd")
			}
		case "apt-get":
			if data, err := os.ReadFile(early); err != nil || string(data) != "early" {
				t.Fatal("early file not ready before packages")
			}
			if _, err := os.Stat(late); !os.IsNotExist(err) {
				t.Fatal("deferred file written before packages")
			}
		case "late-command":
			if data, err := os.ReadFile(late); err != nil || string(data) != "late" {
				t.Fatal("deferred file not ready before runcmd")
			}
		}
		return nil
	}}
	plan, err := Build(c, testOptions(r, "debian"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bootcmd[0]", "write_files[1]", "package_update", "packages", "write_files[0]", "runcmd[0]"}
	if !reflect.DeepEqual(plan.Names(), want) {
		t.Fatalf("step order: %v", plan.Names())
	}
	if _, err := os.Stat(early); !os.IsNotExist(err) || len(r.calls) != 0 {
		t.Fatal("building plan caused side effects")
	}
	if err := plan.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(early)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("file permissions: %v, %v", info, err)
	}
	if got := r.calls[len(r.calls)-1].args; !reflect.DeepEqual(got, c.RunCmd[0].Args) {
		t.Fatalf("argv changed: %#v", got)
	}
}

func TestPackageAdapterCommands(t *testing.T) {
	for _, distro := range []string{"debian", "ubuntu", "alpine"} {
		t.Run(distro, func(t *testing.T) {
			r := &recorder{}
			c := &config.Config{
				PackageUpgrade: true,
				Packages:       []config.Package{{Name: "curl"}, {Name: "example", Version: "1.0-r2"}},
			}
			plan, err := Build(c, testOptions(r, distro))
			if err != nil {
				t.Fatal(err)
			}
			if err := plan.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			var expected []invocation
			if distro == "alpine" {
				expected = []invocation{
					{[]string{"apk", "update"}, nil},
					{[]string{"apk", "upgrade"}, nil},
					{[]string{"apk", "add", "--", "curl", "example=1.0-r2"}, nil},
				}
			} else {
				env := []string{"DEBIAN_FRONTEND=noninteractive"}
				expected = []invocation{
					{[]string{"apt-get", "update"}, env},
					{[]string{"apt-get", "upgrade", "-y"}, env},
					{[]string{"apt-get", "install", "-y", "--", "curl", "example=1.0-r2"}, env},
				}
			}
			if !reflect.DeepEqual(r.calls, expected) {
				t.Fatalf("package operations: %#v", r.calls)
			}
		})
	}
}

func TestPreflightFailuresHaveNoSideEffects(t *testing.T) {
	for _, failure := range []string{"non-root", "unsupported distro", "missing package tool", "missing shell", "unknown owner"} {
		t.Run(failure, func(t *testing.T) {
			r := &recorder{}
			c := &config.Config{BootCmd: []config.Command{{Args: []string{"must-not-run"}}}, PackageUpdate: true}
			opts := testOptions(r, "debian")
			switch failure {
			case "non-root":
				opts.EUID = func() int { return 1000 }
			case "unsupported distro":
				opts.DistroID = func() (string, error) { return "unsupported", nil }
			case "missing package tool":
				opts.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
			case "missing shell":
				c.RunCmd = []config.Command{{Shell: true, Text: "must not run"}}
				opts.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
			case "unknown owner":
				c.WriteFiles = []config.File{{Path: filepath.Join(t.TempDir(), "x"), Owner: "container-init-test-user-does-not-exist:root"}}
			}
			if _, err := Build(c, opts); err == nil {
				t.Fatal("expected preflight failure")
			}
			if len(r.calls) != 0 {
				t.Fatal("preflight executed a command")
			}
		})
	}
}

func TestFailureStopsLaterStepsWithoutLoggingCommands(t *testing.T) {
	r := &recorder{hook: func([]string) error { return errors.New("failed") }}
	c := &config.Config{RunCmd: []config.Command{
		{Shell: true, Text: "secret-command"},
		{Args: []string{"must-not-run"}},
	}}
	var log strings.Builder
	opts := testOptions(r, "debian")
	opts.Log = &log
	plan, err := Build(c, opts)
	if err != nil {
		t.Fatal(err)
	}
	err = plan.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "runcmd[0]") || len(r.calls) != 1 {
		t.Fatalf("failure handling: %v, %#v", err, r.calls)
	}
	if strings.Contains(log.String(), "secret-command") {
		t.Fatal("script logged")
	}
}

func TestFilesAppendAndTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	f := config.File{Path: path, Content: []byte("one"), Permissions: 0600}
	for _, operation := range []struct {
		content string
		append  bool
		want    string
	}{{"one", false, "one"}, {"two", true, "onetwo"}, {"three", false, "three"}} {
		f.Content, f.Append = []byte(operation.content), operation.append
		if err := writeFile(f, os.Geteuid(), os.Getegid()); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != operation.want {
			t.Fatalf("write behavior: %q, %v", got, err)
		}
	}
}

func TestCancelledPlanDoesNotExecute(t *testing.T) {
	r := &recorder{}
	plan, err := Build(&config.Config{BootCmd: []config.Command{{Args: []string{"no"}}}}, testOptions(r, "debian"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := plan.Run(ctx); !errors.Is(err, context.Canceled) || len(r.calls) != 0 {
		t.Fatalf("cancel handling: %v, %v", err, r.calls)
	}
}
