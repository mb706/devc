package boxside

func int80(trap, a1, a2, a3 uintptr) uintptr

// i386SysSocketcall is socketcall's number in the 32-bit x86 table.
const i386SysSocketcall = 102

// probeSocketcall issues socketcall(0, NULL) through the 32-bit entry. The
// kernel refuses call 0 with EINVAL once the call is allowed; the seccomp
// profile answers EPERM first. It returns the errno (0 for success).
func probeSocketcall() (errno int, supported bool) {
	r := int32(uint32(int80(i386SysSocketcall, 0, 0, 0)))
	if r < 0 {
		return int(-r), true
	}
	return 0, true
}
