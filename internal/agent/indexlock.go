package agent

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// indexLockHeld tells whether another process holds the lock of the index
// at path (lockHeld). Tests replace it.
var indexLockHeld = lockHeld

// lockHeld tells whether another process holds a write lock on the file at
// path, as apt's GetLock takes one on its lock files — a POSIX record lock
// over the whole file (fcntl F_SETLK, F_WRLCK) for as long as apt works —,
// without taking anything: fcntl F_GETLK answers with a lock that would keep
// the agent from taking one, and the agent takes none there itself. A file
// that is not there is held by nobody (apt makes it as it locks); one that
// is no plain file, or that the agent cannot open or ask, is an error. The
// file is opened for reading only, and no link is followed.
func lockHeld(path string) (bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer unix.Close(fd) //nolint:errcheck // opened for reading
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, &os.PathError{Op: "open", Path: path, Err: errors.New("not a plain file")}
	}
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: unix.SEEK_SET}
	if err := unix.FcntlFlock(uintptr(fd), unix.F_GETLK, &lk); err != nil {
		return false, &os.PathError{Op: "fcntl F_GETLK", Path: path, Err: err}
	}
	return lk.Type != unix.F_UNLCK, nil
}
