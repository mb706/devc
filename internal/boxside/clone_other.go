//go:build !s390x

package boxside

// cloneFlagArgs places the flags in the first argument of clone.
func cloneFlagArgs(flags uintptr) (uintptr, uintptr) { return flags, 0 }
