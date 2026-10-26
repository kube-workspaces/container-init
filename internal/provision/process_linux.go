package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type ProcessRunner struct {
	Stdout io.Writer
	Stderr io.Writer
}

// Run never invokes a shell implicitly. Each provisioning command gets a process
// group so cancellation also terminates descendants, not just the direct child.
func (r ProcessRunner) Run(ctx context.Context, args, extraEnv []string) error {
	return r.run(ctx, args, extraEnv, nil, false)
}

// RunInput keeps passwords out of argv, environment and external-tool output.
func (r ProcessRunner) RunInput(ctx context.Context, args []string, input io.Reader) error {
	return r.run(ctx, args, nil, input, true)
}

func (r ProcessRunner) run(ctx context.Context, args, extraEnv []string, input io.Reader, sensitive bool) error {
	if len(args) == 0 || args[0] == "" {
		return errors.New("missing executable")
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = r.Stdout, r.Stderr
	cmd.Stdin = input
	if sensitive {
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	}
	cmd.Env = mergeEnv(os.Environ(), extraEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("command exited unsuccessfully (%s)", exit.String())
		}
		// Do not echo argv/script text embedded in exec errors.
		return errors.New("could not start command; check executable and permissions")
	}
	return nil
}

func mergeEnv(base, extra []string) []string {
	result := append([]string(nil), base...)
	for _, value := range extra {
		name, _, _ := strings.Cut(value, "=")
		filtered := result[:0]
		for _, current := range result {
			key, _, _ := strings.Cut(current, "=")
			if key != name {
				filtered = append(filtered, current)
			}
		}
		result = append(filtered, value)
	}
	return result
}
