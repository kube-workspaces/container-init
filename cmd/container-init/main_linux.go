// container-init provisions a container before handing off to its application.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, handoff func([]string) error) (runErr error) {
	flags := flag.NewFlagSet("container-init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var path, defaultUser, runAs, statusFile, terminationMessage string
	var validate, dryRun, showVersion, showKeys bool
	phase := "preflight"
	defer func() {
		if runErr != nil && terminationMessage != "" && !validate && !dryRun {
			data, _ := json.Marshal(struct {
				Source  string `json:"source"`
				Version int    `json:"version"`
				State   string `json:"state"`
				Step    string `json:"step"`
			}{"container-init", 1, "failed", phase})
			// Kubernetes mounts this file separately, so atomic rename is not possible.
			if err := os.WriteFile(terminationMessage, data, 0600); err != nil {
				fmt.Fprintln(stderr, "container-init: could not write termination diagnostic")
			}
		}
	}()
	flags.StringVar(&path, "user-data", "", "cloud-config file path, or - for stdin (required)")
	flags.StringVar(&defaultUser, "default-user", "", "explicit account for users: default and top-level SSH keys")
	flags.StringVar(&runAs, "run-as", "", "drop to this non-root account after provisioning and before application exec")
	flags.StringVar(&statusFile, "status-file", "", "write atomic diagnostic JSON; never skips provisioning")
	flags.StringVar(&terminationMessage, "termination-message", "", "write payload-free failure JSON to this Kubernetes termination-message file")
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
	if defaultUser != "" && !config.ValidAccountName(defaultUser) {
		return errors.New("invalid --default-user")
	}
	if runAs != "" && (runAs == "root" || !config.ValidAccountName(runAs) || (!validate && !dryRun && len(flags.Args()) == 0)) {
		return errors.New("--run-as requires a non-root account name and application argv")
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
	data, err := io.ReadAll(io.LimitReader(input, config.MaxDocumentBytes+1))
	if err != nil {
		return errors.New("cannot read user-data")
	}
	c, err := config.Parse(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if validate {
		fmt.Fprintln(stdout, "cloud-config is valid for the implemented subset")
		return nil
	}
	if runAs != "" && os.Geteuid() != 0 {
		return errors.New("--run-as requires root to replace application credentials")
	}
	var status func(string, string) error
	if statusFile != "" && !dryRun {
		status = provision.StatusWriter(statusFile, fmt.Sprintf("%x", sha256.Sum256(data)))
	}
	plan, err := provision.Build(c, provision.Options{
		Commands:    provision.ProcessRunner{Stdout: stdout, Stderr: stderr},
		Log:         stderr,
		DefaultUser: defaultUser,
		Status: func(state, step string) error {
			if state == "running" && step != "" {
				phase = step
			}
			if state == "succeeded" {
				phase = "handoff"
			}
			if status != nil {
				return status(state, step)
			}
			return nil
		},
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
		if runAs != "" {
			if err := provision.DropPrivileges(runAs); err != nil {
				return err
			}
		}
		return handoff(flags.Args())
	}
	return nil
}
