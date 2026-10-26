// Package provision builds and runs ordered container-local provisioning steps.
package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/kube-workspaces/container-init/internal/config"
)

// Commander is injectable so package-manager behavior can be tested without
// installing packages or mutating the developer's host.
type Commander interface {
	Run(context.Context, []string, []string) error
}

type Options struct {
	Commands Commander
	LookPath func(string) (string, error)
	DistroID func() (string, error)
	EUID     func() int
	Log      io.Writer
}

type Step struct {
	Name string
	run  func(context.Context) error
}

type Plan struct {
	steps []Step
	log   io.Writer
}

func (p *Plan) Names() []string {
	result := make([]string, 0, len(p.steps))
	for _, step := range p.steps {
		result = append(result, step.Name)
	}
	return result
}

// Build preflights available host tools and early file owners before any step.
// Executables/owners created by provisioning are resolved at their execution
// phase instead (late command binaries and deferred file owners, for example).
func Build(c *config.Config, opts Options) (*Plan, error) {
	if c == nil {
		return nil, errors.New("configuration is required")
	}
	if opts.Commands == nil {
		return nil, errors.New("command runner is required")
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.DistroID == nil {
		opts.DistroID = distroID
	}
	if opts.EUID == nil {
		opts.EUID = os.Geteuid
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	p := &Plan{log: opts.Log}
	for _, commands := range [][]config.Command{c.BootCmd, c.RunCmd} {
		for _, command := range commands {
			if command.Shell {
				if _, err := opts.LookPath("/bin/sh"); err != nil {
					return nil, errors.New("shell commands require /bin/sh")
				}
			}
		}
	}
	addCommands := func(name string, commands []config.Command) {
		for i, command := range commands {
			if command.Skip {
				continue
			}
			p.steps = append(p.steps, Step{fmt.Sprintf("%s[%d]", name, i), func(ctx context.Context) error {
				return opts.Commands.Run(ctx, command.Argv(), nil)
			}})
		}
	}
	addFiles := func(deferred bool) error {
		for i, file := range c.WriteFiles {
			if file.Defer != deferred {
				continue
			}
			var uid, gid int
			var err error
			if !deferred {
				uid, gid, err = resolveOwner(file.Owner)
				if err != nil {
					return fmt.Errorf("write_files[%d]: %w", i, err)
				}
				if opts.EUID() != 0 && (uid != opts.EUID() || gid != os.Getegid()) {
					return fmt.Errorf("write_files[%d]: changing ownership requires root", i)
				}
			}
			p.steps = append(p.steps, Step{fmt.Sprintf("write_files[%d]", i), func(context.Context) error {
				if deferred {
					uid, gid, err = resolveOwner(file.Owner)
					if err != nil {
						return err
					}
				}
				return writeFile(file, uid, gid)
			}})
		}
		return nil
	}
	addCommands("bootcmd", c.BootCmd)
	if err := addFiles(false); err != nil {
		return nil, err
	}
	if c.PackageUpdate || c.PackageUpgrade || len(c.Packages) > 0 {
		if opts.EUID() != 0 {
			return nil, errors.New("package provisioning requires root")
		}
		id, err := opts.DistroID()
		if err != nil {
			return nil, err
		}
		manager, steps, err := packageSteps(id, c)
		if err != nil {
			return nil, err
		}
		if _, err := opts.LookPath(manager); err != nil {
			return nil, fmt.Errorf("package provisioning requires %s in the image", manager)
		}
		for _, step := range steps {
			p.steps = append(p.steps, Step{step.name, func(ctx context.Context) error {
				return opts.Commands.Run(ctx, step.args, step.env)
			}})
		}
	}
	if err := addFiles(true); err != nil {
		return nil, err
	}
	addCommands("runcmd", c.RunCmd)
	return p, nil
}

func (p *Plan) Run(ctx context.Context) error {
	for _, step := range p.steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Only phase labels are logged, not scripts, args, file content or passwords.
		fmt.Fprintf(p.log, "container-init: %s\n", step.Name)
		if err := step.run(ctx); err != nil {
			return fmt.Errorf("%s: %w", step.Name, err)
		}
	}
	return ctx.Err()
}

type packageStep struct {
	name string
	args []string
	env  []string
}

func packageSteps(id string, c *config.Config) (string, []packageStep, error) {
	var manager string
	var env []string
	var update, upgrade, install []string
	switch id {
	case "debian", "ubuntu":
		manager = "apt-get"
		env = []string{"DEBIAN_FRONTEND=noninteractive"}
		update = []string{manager, "update"}
		upgrade = []string{manager, "upgrade", "-y"}
		install = []string{manager, "install", "-y", "--"}
	case "alpine":
		manager = "apk"
		update = []string{manager, "update"}
		upgrade = []string{manager, "upgrade"}
		install = []string{manager, "add", "--"}
	default:
		return "", nil, errors.New("packages require a supported /etc/os-release ID: debian, ubuntu or alpine")
	}
	steps := []packageStep{{"package_update", update, env}}
	if c.PackageUpgrade {
		steps = append(steps, packageStep{"package_upgrade", upgrade, env})
	}
	if len(c.Packages) > 0 {
		for _, pkg := range c.Packages {
			name := pkg.Name
			if pkg.Version != "" {
				name += "=" + pkg.Version
			}
			install = append(install, name)
		}
		steps = append(steps, packageStep{"packages", install, env})
	}
	return manager, steps, nil
}

func distroID() (string, error) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "", errors.New("cannot read /etc/os-release for package-manager selection")
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && key == "ID" {
			return strings.Trim(value, `"'`), nil
		}
	}
	return "", errors.New("/etc/os-release has no ID for package-manager selection")
}
