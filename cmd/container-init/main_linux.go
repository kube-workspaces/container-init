// container-init provisions a container before handing off to its application.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/kube-workspaces/container-init/internal/config"
	"github.com/kube-workspaces/container-init/internal/provision"
)

// Overridden by Makefile/CI through -ldflags "-X main.version=<git version>".
// Direct go build/go run invocations without those flags report dev.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, func(args []string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		path, err := exec.LookPath(args[0])
		if err != nil {
			return errors.New("application executable not found")
		}
		// Restore normal signal disposition before replacing this process. Exec
		// preserves argv, environment, stdio, PID and the application's exit status.
		stop()
		return syscall.Exec(path, args, os.Environ())
	})
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "container-init:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, handoff func([]string) error) error {
	flags := flag.NewFlagSet("container-init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var path string
	var validate, dryRun, showVersion, showKeys bool
	flags.StringVar(&path, "user-data", "", "cloud-config file path, or - for stdin (required)")
	flags.BoolVar(&validate, "validate", false, "validate supported configuration without host checks or mutations")
	flags.BoolVar(&dryRun, "dry-run", false, "validate and preflight, then list steps without executing them")
	flags.BoolVar(&showVersion, "version", false, "print version")
	flags.BoolVar(&showKeys, "list-keys", false, "list implemented and planned configuration keys")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: container-init --user-data FILE [--validate|--dry-run] [-- APP ARG...]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if showVersion {
		fmt.Fprintln(stdout, version)
		return nil
	}
	if showKeys {
		for _, key := range config.Keys() {
			status := "planned"
			if key.Implemented {
				status = "implemented"
			}
			fmt.Fprintf(stdout, "%s\t%s\n", key.Name, status)
		}
		return nil
	}
	if path == "" {
		return errors.New("--user-data is required")
	}
	if validate && dryRun {
		return errors.New("choose --validate or --dry-run")
	}
	if (validate || dryRun) && len(flags.Args()) > 0 {
		return errors.New("validation/dry-run cannot launch an application")
	}
	input := stdin
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open user-data: %w", err)
		}
		defer func() { _ = file.Close() }()
		input = file
	}
	c, err := config.Parse(input)
	if err != nil {
		return err
	}
	if validate {
		fmt.Fprintln(stdout, "cloud-config is valid for the implemented subset")
		return nil
	}
	plan, err := provision.Build(c, provision.Options{
		Commands: provision.ProcessRunner{Stdout: stdout, Stderr: stderr},
		Log:      stderr,
	})
	if err != nil {
		return err
	}
	if dryRun {
		for _, name := range plan.Names() {
			fmt.Fprintln(stdout, name)
		}
		return nil
	}
	if err := plan.Run(ctx); err != nil {
		return err
	}
	if len(flags.Args()) > 0 {
		return handoff(flags.Args())
	}
	return nil
}
