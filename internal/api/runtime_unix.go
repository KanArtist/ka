//go:build unix

package api

import (
	"fmt"
	"os"
	"syscall"
)

// checkPrivateDir verifies that dir is a real directory (not a symlink),
// owned by the current user and not writable by group or others, so another
// user cannot plant or read the runtime file in a shared temporary directory.
func checkPrivateDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("not a directory")
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("owned by uid %d, not by you (uid %d)", sys.Uid, os.Getuid())
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("writable by group or others (mode %v)", st.Mode().Perm())
	}
	return nil
}
