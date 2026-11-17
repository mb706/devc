// devc hub doctor (work order C11b section 4): file permissions of the hub
// directory, the CA key, the password, the public URL and ntfy.

package hub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Finding is one check of the doctor.
type Finding struct {
	Check  string `json:"check"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	// Warn marks a finding that is advice, not a problem.
	Warn bool `json:"warn,omitempty"`
}

// Doctor checks a hub directory. publicName overrides the stored one.
func Doctor(ctx context.Context, l Layout, publicName string) []Finding {
	var out []Finding
	add := func(check string, ok bool, format string, args ...any) {
		out = append(out, Finding{Check: check, OK: ok, Detail: fmt.Sprintf(format, args...)})
	}
	uid := os.Getuid()

	// Permissions: nothing in the hub directory may be readable or
	// writable by others; the directories and secrets not by the group
	// either.
	type want struct {
		path    string
		dir     bool
		secret  bool
		missing string // detail when it may be missing ("" = must exist)
	}
	for _, f := range []want{
		{l.Dir, true, true, ""},
		{filepath.Dir(l.CAKey()), true, true, ""},
		{l.CAKey(), false, true, ""},
		{l.CACert(), false, false, ""},
		{l.DB(), false, true, ""},
		{filepath.Dir(l.ServerKey()), true, true, "no server directory (a certificate from elsewhere or a proxy)"},
		{l.ServerKey(), false, true, "no server key (a certificate from elsewhere or a proxy)"},
		{l.ConfigFile(), false, true, "no hub.toml (defaults; no notifications)"},
	} {
		check := "permissions of " + f.path
		fi, err := os.Lstat(f.path)
		if errors.Is(err, os.ErrNotExist) && f.missing != "" {
			out = append(out, Finding{Check: check, OK: true, Detail: f.missing})
			continue
		}
		if err != nil {
			add(check, false, "%v", err)
			continue
		}
		mode := fi.Mode()
		var problems []string
		switch {
		case mode&os.ModeSymlink != 0:
			problems = append(problems, "is a symbolic link")
		case f.dir && !fi.IsDir():
			problems = append(problems, "is not a directory")
		case !f.dir && !mode.IsRegular():
			problems = append(problems, "is not a regular file")
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
			problems = append(problems, fmt.Sprintf("is owned by uid %d, not %d", st.Uid, uid))
		}
		perm := mode.Perm()
		if perm&0o002 != 0 {
			problems = append(problems, "is world-writable")
		}
		if perm&0o020 != 0 {
			problems = append(problems, "is group-writable")
		}
		if f.secret && perm&0o004 != 0 {
			problems = append(problems, "is world-readable")
		}
		if f.secret && perm&0o040 != 0 {
			problems = append(problems, "is group-readable")
		}
		if len(problems) > 0 {
			add(check, false, "mode %04o: %s (chmod %s)", perm, joinProblems(problems), suggestMode(f.dir, f.secret))
		} else {
			add(check, true, "mode %04o", perm)
		}
	}

	st, err := OpenStore(l.DB())
	if err != nil {
		add("database", false, "%v", err)
		return out
	}
	defer st.Close()
	hash, err := st.PasswordHash()
	switch {
	case err != nil:
		add("password", false, "%v", err)
	case hash == "":
		add("password", false, "no password is set: run 'devc hub user set-password'")
	default:
		add("password", true, "set")
	}
	if secret, _, err := st.TOTP(); err == nil {
		f := Finding{Check: "TOTP", OK: true, Detail: "enabled"}
		if secret == "" {
			f.Detail, f.Warn = "off ('devc hub user totp enable' adds a second factor)", true
		}
		out = append(out, f)
	}

	cfg, err := LoadConfig(l)
	if err != nil {
		add("hub.toml", false, "%v", err)
		return out
	}
	if publicName == "" {
		publicName, _ = st.Meta(metaPublicName)
	}
	switch {
	case publicName == "":
		add("public URL", false, "the hub has no public name ('devc hub init --public-name' or 'serve --public-name')")
	default:
		host, port, err := splitPublicName(publicName)
		if err != nil {
			add("public URL", false, "%v", err)
			break
		}
		u := (&Server{host: host, port: port}).origin()
		if cfg.PublicURL != "" {
			if err := checkPublicURL(cfg.PublicURL, host, port); err != nil {
				add("public URL", false, "%v", err)
				break
			}
			u = cfg.PublicURL
		}
		add("public URL", true, "%s", u)
	}

	if cfg.Ntfy == nil {
		out = append(out, Finding{Check: "ntfy", OK: true, Warn: true, Detail: "not configured: no notifications ([ntfy] in hub.toml)"})
	} else if err := NtfyHealth(ctx, *cfg.Ntfy); err != nil {
		add("ntfy", false, "%s: %v", cfg.Ntfy.URL, err)
	} else {
		add("ntfy", true, "%s answers", cfg.Ntfy.URL)
	}
	return out
}

func joinProblems(ps []string) string {
	s := ""
	for i, p := range ps {
		if i > 0 {
			s += ", "
		}
		s += p
	}
	return s
}

func suggestMode(dir, secret bool) string {
	switch {
	case dir:
		return "700"
	case secret:
		return "600"
	}
	return "644"
}
