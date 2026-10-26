package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidateAndDryRunDoNotExecuteCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "should-not-exist")
	input := fmt.Sprintf("#cloud-config\nruncmd:\n  - [touch, %q]\n", path)
	for _, flag := range []string{"--validate", "--dry-run"} {
		var out, logs bytes.Buffer
		err := run(context.Background(), []string{"--user-data", "-", flag}, strings.NewReader(input), &out, &logs, func([]string) error {
			t.Fatal("validation launched application")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s ran a command", flag)
		}
	}
}

func TestApplicationHandoffAfterProvisioning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	input := fmt.Sprintf("#cloud-config\nruncmd:\n  - [touch, %q]\n", path)
	var out, logs bytes.Buffer
	app := []string{"application", "arg with spaces", "", "$HOME"}
	called := false
	err := run(context.Background(), append([]string{"--user-data", "-", "--"}, app...), strings.NewReader(input), &out, &logs, func(args []string) error {
		called = true
		if _, err := os.Stat(path); err != nil {
			t.Fatal("application started before provisioning")
		}
		if !reflect.DeepEqual(args, app) {
			t.Fatalf("application args changed: %#v", args)
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("handoff: called=%v, err=%v", called, err)
	}
}

func TestInvalidOrFailedConfigNeverLaunchesApplication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-exist")
	for _, input := range []string{
		fmt.Sprintf("#cloud-config\nbootcmd:\n  - [touch, %q]\nchpasswd: {}\n", path),
		fmt.Sprintf("#cloud-config\nbootcmd:\n  - [touch, %q]\nwrite_files: [{path: /run/x, encoding: b64, content: 'invalid!'}]\n", path),
		"#cloud-config\nruncmd: [[sh, -c, 'exit 7']]\n",
	} {
		var out, logs bytes.Buffer
		err := run(context.Background(), []string{"--user-data", "-", "--", "app"}, strings.NewReader(input), &out, &logs, func([]string) error {
			t.Fatal("application launched after invalid/failed config")
			return nil
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("invalid document partially executed")
		}
	}
}
