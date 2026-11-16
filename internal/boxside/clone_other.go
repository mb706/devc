//go:build !s390x

// This file places clone's flags on architectures that pass them first.

package boxside

// cloneFlagArgs places the flags in the first argument of clone.
func cloneFlagArgs(flags uintptr) (uintptr, uintptr) { return flags, 0 }
