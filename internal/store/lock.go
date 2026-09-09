package store

import (
	"fmt"
	"os"
	"syscall"
)

// OpenExclusive opens the DB and takes an exclusive advisory lock on a separate
// lock file (<path>.lock). The daemon needs it: two processes on one DB would
// corrupt the state and status tables. The lock is released in Close.
func OpenExclusive(path string) (*Store, error) {
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating lock file %s.lock: %w", path, err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lf.Close()
		return nil, fmt.Errorf("DB %s is already in use by another process (could not take %s.lock): %w", path, path, err)
	}

	st, err := Open(path)
	if err != nil {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
		return nil, err
	}
	st.lock = lf
	return st, nil
}
