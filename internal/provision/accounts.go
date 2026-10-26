package provision

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/kube-workspaces/container-init/internal/config"
)

type inputCommander interface {
	RunInput(context.Context, []string, io.Reader) error
}

func identitySteps(c *config.Config, opts Options) ([]Step, error) {
	if len(c.Groups)+len(c.Users)+len(c.Passwords)+len(c.SSHKeys)+len(c.CACerts) == 0 && c.Timezone == "" {
		return nil, nil
	}
	if opts.EUID() != 0 {
		return nil, errors.New("account, SSH, CA and timezone provisioning requires root")
	}
	var steps []Step
	var locks []Step
	require := func(tool string) error {
		if _, err := opts.LookPath(tool); err != nil {
			return fmt.Errorf("identity provisioning requires %s already installed in the image", tool)
		}
		return nil
	}
	for i, g := range c.Groups {
		if err := require("groupadd"); err != nil {
			return nil, err
		}
		for _, member := range g.Members {
			if _, err := user.Lookup(member); err != nil {
				return nil, fmt.Errorf("groups[%d]: membership users must already exist", i)
			}
		}
		steps = append(steps, Step{fmt.Sprintf("groups[%d]", i), func(ctx context.Context) error {
			if _, err := user.LookupGroup(g.Name); err != nil {
				var missing user.UnknownGroupError
				if !errors.As(err, &missing) {
					return errors.New("cannot look up group")
				}
				if err := opts.Commands.Run(ctx, []string{"groupadd", "--", g.Name}, nil); err != nil {
					return err
				}
			}
			for _, member := range g.Members {
				if err := opts.Commands.Run(ctx, []string{"usermod", "-a", "-G", g.Name, "--", member}, nil); err != nil {
					return err
				}
			}
			return nil
		}})
		if len(g.Members) > 0 {
			if err := require("usermod"); err != nil {
				return nil, err
			}
		}
	}
	for i, original := range c.Users {
		u := original
		if u.Default {
			if !config.ValidAccountName(opts.DefaultUser) {
				return nil, errors.New("users: default requires --default-user")
			}
			u.Name = opts.DefaultUser
			if _, err := user.Lookup(u.Name); err != nil {
				return nil, errors.New("users: default must resolve to an existing account")
			}
		}
		if err := require("useradd"); err != nil {
			return nil, err
		}
		if err := require("usermod"); err != nil {
			return nil, err
		}
		steps = append(steps, Step{fmt.Sprintf("users[%d]", i), func(ctx context.Context) error {
			account, err := user.Lookup(u.Name)
			if err != nil {
				var missing user.UnknownUserError
				if !errors.As(err, &missing) {
					return errors.New("cannot look up user")
				}
				args := []string{"useradd"}
				if u.NoCreateHome {
					args = append(args, "-M")
				} else {
					args = append(args, "-m")
				}
				if u.System {
					args = append(args, "-r")
				}
				if u.UID != nil {
					args = append(args, "-u", strconv.FormatUint(uint64(*u.UID), 10))
				}
				if u.Home != "" {
					args = append(args, "-d", u.Home)
				}
				if u.Shell != "" {
					args = append(args, "-s", u.Shell)
				}
				if u.PrimaryGroup != "" {
					args = append(args, "-g", u.PrimaryGroup)
				} else {
					args = append(args, "-U")
				}
				if err := opts.Commands.Run(ctx, append(args, "--", u.Name), nil); err != nil {
					return err
				}
			} else {
				if u.UID != nil && account.Uid != strconv.FormatUint(uint64(*u.UID), 10) {
					return errors.New("existing user UID differs from requested UID")
				}
				if u.Home != "" && account.HomeDir != u.Home {
					return errors.New("existing user home differs from requested home")
				}
				if u.PrimaryGroup != "" {
					g, err := user.LookupGroup(u.PrimaryGroup)
					if err != nil || g.Gid != account.Gid {
						return errors.New("existing user primary group differs from requested group")
					}
				}
				if u.Shell != "" {
					data, err := os.ReadFile("/etc/passwd")
					if err != nil {
						return errors.New("cannot inspect existing user shell")
					}
					found := false
					for _, line := range strings.Split(string(data), "\n") {
						fields := strings.Split(line, ":")
						if len(fields) == 7 && fields[0] == u.Name {
							found = fields[6] == u.Shell
						}
					}
					if !found {
						return errors.New("existing user shell differs from requested shell")
					}
				}
			}
			if len(u.Groups) > 0 {
				if err := opts.Commands.Run(ctx, []string{"usermod", "-a", "-G", strings.Join(u.Groups, ","), "--", u.Name}, nil); err != nil {
					return err
				}
			}
			if len(u.SSHKeys) > 0 {
				return authorizedKeys(u.Name, u.SSHKeys)
			}
			return nil
		}})
		if u.LockPasswd != nil {
			locks = append(locks, Step{fmt.Sprintf("users[%d].lock_passwd", i), func(ctx context.Context) error {
				flag := "-L"
				if !*u.LockPasswd {
					flag = "-U"
				}
				if err := opts.Commands.Run(ctx, []string{"usermod", flag, "--", u.Name}, nil); err != nil {
					return err
				}
				if !*u.LockPasswd {
					data, err := os.ReadFile("/etc/shadow")
					if err != nil {
						return errors.New("cannot verify password unlock")
					}
					for _, line := range strings.Split(string(data), "\n") {
						fields := strings.Split(line, ":")
						if len(fields) > 1 && fields[0] == u.Name && fields[1] != "" && !strings.HasPrefix(fields[1], "!") && !strings.HasPrefix(fields[1], "*") {
							return nil
						}
					}
					return errors.New("unlock requires an existing password hash or a chpasswd.users password; passwordless accounts are unsupported")
				}
				return nil
			}})
		}
	}
	if len(c.Passwords) > 0 {
		if err := require("chpasswd"); err != nil {
			return nil, err
		}
		if c.ExpirePasswords {
			if err := require("chage"); err != nil {
				return nil, err
			}
		}
		runner, ok := opts.Commands.(inputCommander)
		if !ok {
			return nil, errors.New("password provisioning requires a stdin-capable command runner")
		}
		for i, p := range c.Passwords {
			steps = append(steps, Step{fmt.Sprintf("chpasswd.users[%d]", i), func(ctx context.Context) error {
				if _, err := user.Lookup(p.Name); err != nil {
					return errors.New("password user does not exist")
				}
				args := []string{"chpasswd"}
				if p.Hashed {
					args = append(args, "-e")
				}
				if err := runner.RunInput(ctx, args, strings.NewReader(p.Name+":"+p.Value+"\n")); err != nil {
					return err
				}
				if c.ExpirePasswords {
					return opts.Commands.Run(ctx, []string{"chage", "-d", "0", "--", p.Name}, nil)
				}
				return nil
			}})
		}
	}
	steps = append(steps, locks...)
	if len(c.SSHKeys) > 0 {
		if !config.ValidAccountName(opts.DefaultUser) {
			return nil, errors.New("ssh_authorized_keys requires --default-user")
		}
		steps = append(steps, Step{"ssh_authorized_keys", func(context.Context) error { return authorizedKeys(opts.DefaultUser, c.SSHKeys) }})
	}
	if len(c.CACerts) > 0 {
		if err := require("update-ca-certificates"); err != nil {
			return nil, err
		}
		steps = append(steps, Step{"ca_certs", func(ctx context.Context) error {
			for _, cert := range c.CACerts {
				sum := sha256.Sum256([]byte(cert))
				if err := os.MkdirAll("/usr/local/share/ca-certificates", 0755); err != nil {
					return err
				}
				if err := atomicFile(fmt.Sprintf("/usr/local/share/ca-certificates/container-init-%x.crt", sum[:]), []byte(cert), 0644); err != nil {
					return err
				}
			}
			return opts.Commands.Run(ctx, []string{"update-ca-certificates"}, nil)
		}})
	}
	if c.Timezone != "" {
		zone, err := os.ReadFile(filepath.Join("/usr/share/zoneinfo", c.Timezone))
		if err != nil || len(zone) < 4 || string(zone[:4]) != "TZif" {
			return nil, errors.New("timezone requires installed IANA zoneinfo data for the selected zone")
		}
		steps = append(steps, Step{"timezone", func(context.Context) error {
			if err := atomicFile("/etc/localtime", zone, 0644); err != nil {
				return err
			}
			return atomicFile("/etc/timezone", []byte(c.Timezone+"\n"), 0644)
		}})
	}
	return steps, nil
}

func atomicFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".container-init-*")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func authorizedKeys(name string, keys []string) error {
	u, err := user.Lookup(name)
	if err != nil {
		return errors.New("SSH user does not exist")
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return errors.New("invalid SSH user UID")
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return errors.New("invalid SSH user GID")
	}
	if !filepath.IsAbs(u.HomeDir) || u.HomeDir == "/" {
		return errors.New("SSH user requires an absolute non-root home directory")
	}
	dir := filepath.Join(u.HomeDir, ".ssh")
	if err = os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return errors.New("cannot create SSH directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("SSH directory must be a real directory")
	}
	if err = os.Chown(dir, uid, gid); err != nil {
		return err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "authorized_keys"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("cannot open authorized_keys")
	}
	defer func() { _ = f.Close() }()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("authorized_keys must be a regular file")
	}
	if err = f.Chown(uid, gid); err != nil {
		return err
	}
	if err = f.Chmod(0600); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("cannot read authorized_keys within 1 MiB limit")
	}
	seen := map[string]bool{}
	identity := func(line string) string {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			return parts[0] + " " + parts[1]
		}
		return line
	}
	for _, line := range strings.Split(string(data), "\n") {
		seen[identity(line)] = true
	}
	var appended strings.Builder
	if len(data) > 0 && data[len(data)-1] != '\n' {
		appended.WriteByte('\n')
	}
	for _, key := range keys {
		if !seen[identity(key)] {
			appended.WriteString(key + "\n")
			seen[identity(key)] = true
		}
	}
	_, err = f.WriteString(appended.String())
	return err
}
