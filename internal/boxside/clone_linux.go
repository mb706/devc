// This file holds rawClone, the raw clone(2) call of the self-test's
// userns-clone item.

package boxside

import "golang.org/x/sys/unix"

// rawClone issues clone(flags, 0, …). It is only called with flag
// combinations the kernel refuses before creating a process, so no child
// can ever run Go code here.
func rawClone(flags uintptr) error {
	a0, a1 := cloneFlagArgs(flags)
	_, _, e := unix.RawSyscall6(unix.SYS_CLONE, a0, a1, 0, 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
