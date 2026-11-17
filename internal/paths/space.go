// This file holds the free-space reserve of the state file system.

package paths

import "golang.org/x/sys/unix"

// StateMinFree is the free space devc keeps on the state file system:
// work a box causes there (a push, a bank's fetch, events ingested into
// a journal) does not start, or stops, when it would leave less (finding
// D1-05, C20a). The push watchdog ends a transfer that leaves less than
// half of it.
const StateMinFree = 1 << 30

// FreeSpace is the space available to an unprivileged user on the file
// system that holds path.
func FreeSpace(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
