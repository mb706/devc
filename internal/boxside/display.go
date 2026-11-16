package boxside

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mb706/devc/internal/display"
)

// The desktop sidecar (work order C19). While one runs, the host keeps
// /etc/devc/display.env with DISPLAY and XAUTHORITY for programs in the box;
// `devc box launch` and the login shell of `devc sh` add both to the
// environment when the file exists. The cookie itself is in /run/devc.
const (
	DisplayEnvName = "display.env"
	DisplayEnvFile = EtcDir + "/" + DisplayEnvName
)

var (
	displayValueRE = regexp.MustCompile(`^[A-Za-z0-9.-]{0,63}:[0-9]{1,4}(\.[0-9]{1,2})?$`)
	xauthValueRE   = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,255}$`)
)

// DisplayEnv renders display.env.
func DisplayEnv(displayName, xauthority string) []byte {
	return []byte("DISPLAY=" + displayName + "\nXAUTHORITY=" + xauthority + "\n")
}

// ReadDisplayEnv returns the variables of display.env below root ("/" in a
// box) as "KEY=value" strings, or nil when the file does not exist. The
// file is the host's; it is parsed strictly all the same.
func ReadDisplayEnv(root string) ([]string, error) {
	b, err := readCapped(filepath.Join(root, DisplayEnvFile), 1024)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		switch {
		case !ok || seen[k]:
			return nil, fmt.Errorf("%s: bad line %q", DisplayEnvFile, line)
		case k == "DISPLAY" && displayValueRE.MatchString(v), k == "XAUTHORITY" && xauthValueRE.MatchString(v):
		default:
			return nil, fmt.Errorf("%s: bad line %q", DisplayEnvFile, line)
		}
		seen[k] = true
		out = append(out, line)
	}
	if !seen["DISPLAY"] || !seen["XAUTHORITY"] {
		return nil, fmt.Errorf("%s lacks DISPLAY or XAUTHORITY", DisplayEnvFile)
	}
	return out, nil
}

// WithDisplay returns environ with the variables of display.env set (they
// replace values already there); environ is returned unchanged when the
// file does not exist or cannot be read.
func WithDisplay(root string, environ []string) []string {
	vars, err := ReadDisplayEnv(root)
	if err != nil || vars == nil {
		return environ
	}
	env := newEnvList(environ)
	for _, kv := range vars {
		k, v, _ := strings.Cut(kv, "=")
		env.set(k, v)
	}
	return env.list()
}

// XCheckResult is the outcome of XCheck.
type XCheckResult struct {
	Display string
	Setup   *display.Setup
}

// XCheck opens the display named by DISPLAY in environ (or in display.env)
// and performs the X11 connection setup with the cookie from XAUTHORITY
// (likewise), or with no authorisation when noAuth is set or no cookie is
// found. A display that refuses returns a *display.RefusedError.
func XCheck(root string, environ []string, noAuth bool, timeout time.Duration) (*XCheckResult, error) {
	env := newEnvList(WithDisplay(root, nil))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && (k == "DISPLAY" || k == "XAUTHORITY" || k == "HOME") && v != "" {
			env.set(k, v)
		}
	}
	name := env.get("DISPLAY")
	if name == "" {
		return nil, errors.New("no display: DISPLAY is not set and no desktop runs for this box (see 'devc gui up' on the host)")
	}
	host, rest, ok := strings.Cut(name, ":")
	numStr, _, _ := strings.Cut(rest, ".")
	number, err := strconv.Atoi(numStr)
	if !ok || err != nil || number < 0 || number > 9999 {
		return nil, fmt.Errorf("cannot parse DISPLAY=%q", name)
	}
	var conn net.Conn
	var addr []byte
	if host == "" || host == "unix" {
		sock := "/tmp/.X11-unix/X" + strconv.Itoa(number)
		if conn, err = net.DialTimeout("unix", "@"+sock, timeout); err != nil {
			conn, err = net.DialTimeout("unix", sock, timeout)
		}
	} else {
		if ip := net.ParseIP(host).To4(); ip != nil {
			addr = ip
		}
		conn, err = net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(6000+number)), timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to display %s: %w", name, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	var authName string
	var authData []byte
	if !noAuth {
		file := env.get("XAUTHORITY")
		if file == "" && env.get("HOME") != "" {
			file = filepath.Join(env.get("HOME"), ".Xauthority")
		}
		if b, err := readCapped(file, display.MaxXAuthority); err == nil {
			if entries, err := display.ParseXAuthority(b); err == nil {
				if data, ok := display.FindCookie(entries, number, addr); ok {
					authName, authData = display.CookieName, data
				}
			}
		}
	}
	setup, err := display.Handshake(conn, authName, authData)
	if err != nil {
		return nil, err
	}
	return &XCheckResult{Display: name, Setup: setup}, nil
}
