package provision

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kube-workspaces/container-init/internal/config"
)

func resolveOwner(owner string) (int, int, error) {
	parts := strings.Split(owner, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, 0, errors.New("owner must be user:group")
	}
	uid, err := strconv.Atoi(parts[0])
	if err != nil {
		account, lookupErr := user.Lookup(parts[0])
		if lookupErr != nil {
			return 0, 0, errors.New("owner user does not exist")
		}
		uid, err = strconv.Atoi(account.Uid)
	}
	if err != nil || uid < 0 {
		return 0, 0, errors.New("invalid owner UID")
	}
	gid, err := strconv.Atoi(parts[1])
	if err != nil {
		group, lookupErr := user.LookupGroup(parts[1])
		if lookupErr != nil {
			return 0, 0, errors.New("owner group does not exist")
		}
		gid, err = strconv.Atoi(group.Gid)
	}
	if err != nil || gid < 0 {
		return 0, 0, errors.New("invalid owner GID")
	}
	return uid, gid, nil
}

func writeFile(file config.File, uid, gid int) error {
	// Track only newly created parents; existing directory ownership is preserved.
	var missing []string
	for dir := filepath.Dir(file.Path); ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return errors.New("file parent is not a directory")
			}
			break
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect file parent: %w", err)
		}
		missing = append(missing, dir)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0755); err != nil {
			return fmt.Errorf("create file parent: %w", err)
		}
		if err := os.Chown(missing[i], uid, gid); err != nil {
			return fmt.Errorf("set parent ownership: %w", err)
		}
	}
	flags := os.O_CREATE | os.O_WRONLY
	if file.Append {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	handle, err := os.OpenFile(file.Path, flags, 0600)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = handle.Close() }()
	if err := handle.Chown(uid, gid); err != nil {
		return fmt.Errorf("set file ownership: %w", err)
	}
	if err := handle.Chmod(os.FileMode(file.Permissions)); err != nil {
		return fmt.Errorf("set file permissions: %w", err)
	}
	if _, err := handle.Write(file.Content); err != nil {
		return fmt.Errorf("write content: %w", err)
	}
	return handle.Close()
}
