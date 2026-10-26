package provision

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProcessPreservesArgvAndOverridesEnvironment(t *testing.T) {
	t.Setenv("CONTAINER_INIT_TEST", "original")
	var output bytes.Buffer
	r := ProcessRunner{Stdout: &output}
	err := r.Run(context.Background(), []string{"/bin/sh", "-c", `printf '%s|%s|%s' "$1" "$2" "$CONTAINER_INIT_TEST"`, "sh", "literal $HOME", ""}, []string{"CONTAINER_INIT_TEST=replaced"})
	if err != nil || output.String() != "literal $HOME||replaced" {
		t.Fatalf("argv/env: %q, %v", output.String(), err)
	}
}

func TestProcessFailureDoesNotEchoArgv(t *testing.T) {
	r := ProcessRunner{}
	for _, args := range [][]string{
		{"/bin/sh", "-c", "exit 7", "sensitive-argument"},
		{"/not/a/real/executable", "sensitive-argument"},
	} {
		err := r.Run(context.Background(), args, nil)
		if err == nil || strings.Contains(err.Error(), "sensitive-argument") {
			t.Fatalf("failure redaction: %v", err)
		}
	}
}

func TestProcessCancellationKillsDescendants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child-survived")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := ProcessRunner{}
	// A surviving background child would create this file after cancellation.
	err := r.Run(ctx, []string{"/bin/sh", "-c", `(sleep 0.4; touch "$1") & wait`, "sh", path}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline cancellation, got %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("background descendant survived cancellation")
	}
}
