// This file places clone's flags on s390x, which passes them second.

package boxside

// cloneFlagArgs places the flags in the second argument of clone: s390
// passes the stack first.
func cloneFlagArgs(flags uintptr) (uintptr, uintptr) { return 0, flags }
