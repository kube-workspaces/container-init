package provision

import (
	"errors"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"syscall"
)

// DropPrivileges is called after provisioning and immediately before exec.
// Supplementary groups come from the resolved account, never the root caller.
func DropPrivileges(name string) error {
	u, err := user.Lookup(name)
	if err != nil {
		return errors.New("application user does not exist")
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil || uid <= 0 {
		return errors.New("run-as requires a non-root user")
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil || gid < 0 {
		return errors.New("invalid application group")
	}
	groupIDs, err := u.GroupIds()
	if err != nil {
		return errors.New("cannot resolve application groups")
	}
	groups := make([]int, 0, len(groupIDs))
	for _, id := range groupIDs {
		g, e := strconv.Atoi(id)
		if e != nil || g < 0 {
			return errors.New("invalid supplementary group")
		}
		groups = append(groups, g)
	}
	if os.Geteuid() != 0 {
		return errors.New("run-as requires root to replace supplementary groups")
	}
	// prctl is thread-local. Keep this goroutine on the same OS thread through
	// the caller's immediate exec so no-new-privileges reaches the application.
	runtime.LockOSThread()
	// Prevent set-ID executables and file capabilities from regaining privilege.
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 38, 1, 0, 0, 0, 0); errno != 0 {
		return errors.New("cannot set no-new-privileges")
	}
	if err = syscall.Setgroups(groups); err != nil {
		return errors.New("cannot set application supplementary groups")
	}
	if err = syscall.Setgid(gid); err != nil {
		return errors.New("cannot set application GID")
	}
	if err = syscall.Setuid(uid); err != nil {
		return errors.New("cannot set application UID")
	}
	for key, value := range map[string]string{"HOME": u.HomeDir, "USER": u.Username, "LOGNAME": u.Username} {
		if err = os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}
