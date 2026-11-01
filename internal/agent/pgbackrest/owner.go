package pgbackrest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"NYCU-SDC/caravanserai/internal/agent/docker"
)

// Owner is who a file must belong to: the user pgBackRest runs as inside the
// container that mounts it. In the cara-postgres image that is 999:999.
type Owner struct {
	UID, GID int
}

// ImageOwner is the postgres user of the cara-postgres image.
var ImageOwner = Owner{UID: 999, GID: 999}

// helperRunner runs a throwaway container; *docker.DockerRuntime satisfies it.
type helperRunner interface {
	RunToCompletion(ctx context.Context, image, user string, binds, cmd []string) (docker.ExecResult, error)
}

// Chowner hands files to a container's user.
//
// It tries chown directly, which works when the Agent is root, and otherwise
// asks Docker to do it from a throwaway root container. An Agent that can
// control Docker can already do anything a root process can, so the second
// path grants nothing new; it only makes a non-root Agent work.
type Chowner struct {
	helpers helperRunner
	image   string
}

// NewChowner returns a Chowner whose fallback container runs image.
func NewChowner(helpers helperRunner, image string) *Chowner {
	return &Chowner{helpers: helpers, image: image}
}

// Give makes owner the owner of every path. Every path must be inside one
// directory, which the fallback mounts.
func (c *Chowner) Give(ctx context.Context, owner Owner, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	var denied bool
	for _, p := range paths {
		if err := os.Lchown(p, owner.UID, owner.GID); err != nil {
			if !errors.Is(err, syscall.EPERM) {
				return fmt.Errorf("pgbackrest: give %s to UID %d: %w", p, owner.UID, err)
			}
			denied = true
			break
		}
	}
	if !denied {
		return nil
	}
	if c.helpers == nil {
		return fmt.Errorf("pgbackrest: give files to UID %d: the Agent is not root and has no Docker fallback", owner.UID)
	}

	dir := filepath.Dir(paths[0])
	cmd := []string{"chown", strconv.Itoa(owner.UID) + ":" + strconv.Itoa(owner.GID)}
	for _, p := range paths {
		if filepath.Dir(p) != dir {
			return fmt.Errorf("pgbackrest: %s is not in %s", p, dir)
		}
		cmd = append(cmd, "/target/"+filepath.Base(p))
	}
	result, err := c.helpers.RunToCompletion(ctx, c.image, "0", []string{dir + ":/target"}, cmd)
	if err != nil {
		return fmt.Errorf("pgbackrest: give files in %s to UID %d: %w", dir, owner.UID, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("pgbackrest: give files in %s to UID %d: chown exited %d: %s", dir, owner.UID, result.ExitCode, result.Stderr)
	}
	return verifyOwner(owner, paths)
}

// verifyOwner checks the result rather than trusting the helper's exit code.
func verifyOwner(owner Owner, paths []string) error {
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("pgbackrest: check owner of %s: %w", p, err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != owner.UID || int(st.Gid) != owner.GID {
			return fmt.Errorf("pgbackrest: %s does not belong to UID %d after chown", p, owner.UID)
		}
	}
	return nil
}
