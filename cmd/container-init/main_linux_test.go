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
		fmt.Sprintf("#cloud-config\nbootcmd:\n  - [touch, %q]\nchpasswd: {list: unsupported}\n", path),
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

func TestFailureTerminationMessageOmitsPayloads(t *testing.T) {
	for _, input := range []string{"#cloud-config\nchpasswd: {list: do-not-print-me}\n", "#cloud-config\nruncmd: [[sh, -c, 'echo do-not-print-me; exit 7']]\n"} {
		path := filepath.Join(t.TempDir(), "termination")
		var output bytes.Buffer
		err := run(context.Background(), []string{"--user-data", "-", "--termination-message", path}, strings.NewReader(input), &output, &output, func([]string) error { t.Fatal("unexpected handoff"); return nil })
		if err == nil {
			t.Fatal("expected failure")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "do-not-print-me") || !strings.Contains(string(data), `"source":"container-init"`) || !strings.Contains(string(data), `"state":"failed"`) {
			t.Fatalf("invalid termination message %s", data)
		}
	}
}

func TestValidationFailureDoesNotWriteDiagnosticFiles(t *testing.T) {
	for _, mode := range []string{"--validate", "--dry-run"} {
		dir := t.TempDir()
		status := filepath.Join(dir, "status.json")
		termination := filepath.Join(dir, "termination")
		var output bytes.Buffer
		err := run(context.Background(), []string{"--user-data", "-", mode, "--status-file", status, "--termination-message", termination}, strings.NewReader("#cloud-config\nunsupported: do-not-print-me\n"), &output, &output, func([]string) error { t.Fatal("validation handed off"); return nil })
		if err == nil {
			t.Fatal("invalid document accepted")
		}
		for _, path := range []string{status, termination} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("validation mutated diagnostic files")
			}
		}
	}
}

func TestRunAsRootFailsBeforeProvisioning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-exist")
	var output bytes.Buffer
	err := run(context.Background(), []string{"--user-data", "-", "--run-as", "root", "--", "app"}, strings.NewReader(fmt.Sprintf("#cloud-config\nbootcmd: [[touch, %q]]\n", path)), &output, &output, func([]string) error { t.Fatal("root handoff accepted"); return nil })
	if err == nil {
		t.Fatal("root run-as accepted")
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid run-as caused provisioning mutations")
	}
}
