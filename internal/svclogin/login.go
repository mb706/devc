// This file implements the logins of cookie services: the service's login
// command runs in a throwaway container (auth.image, by default the
// installer image) with the network on, every capability dropped and
// nothing mounted but a scratch directory, which holds the other files of
// the service's directory and the login secrets (auth.secrets) as 0600
// files. Its standard output is the cookie jar (ParseLoginOutput); its
// standard error is shown to the user with every secret value removed.
// Every error Run returns is redacted the same way before it is shown,
// logged or stored (finding D2-08). The jar becomes the realm secret
// _jar.<service>.

package svclogin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/flock"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/toolbox"
)

// Limits of a login.
const (
	// LoginTimeout bounds the login command.
	LoginTimeout = 5 * time.Minute
	// loginLockWait bounds the wait for another login of the same service.
	loginLockWait = LoginTimeout + time.Minute
	// maxLoginStderr is what is kept of the command's standard error.
	maxLoginStderr = 64 << 10
	// maxLoginFiles and maxLoginFile bound the files copied from the
	// service's directory.
	maxLoginFiles = 32
	maxLoginFile  = 1 << 20
)

// In-container paths of a login.
const (
	LoginDir        = "/login"
	LoginSecretsDir = "/login/secrets"
)

// Login runs the login command of cookie services.
type Login struct {
	Engine       engine.Engine
	Store        secrets.Store
	Paths        *paths.Paths
	SecurityOpts []string
	// ContextDir is where the installer image is built when it is missing.
	ContextDir string
	// Out receives progress (building the installer image).
	Out io.Writer
	Now func() time.Time
}

// Result is a successful login.
type Result struct {
	Jar *Jar
	// Stderr is the command's standard error, printable and without any
	// secret value.
	Stderr string
}

// cappedBuffer keeps at most max bytes and remembers whether more came.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		b.over = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

// Run logs in to s and stores the jar. Logins of one service are
// serialised across processes (the CLI and the daemon's automatic login).
// A failure's message holds none of the login's secret values.
func (l *Login) Run(ctx context.Context, s *config.Service) (*Result, error) {
	var redact []string
	res, err := l.run(ctx, s, &redact)
	if err != nil {
		return nil, errors.New(cleanStderr(err.Error(), redact))
	}
	return res, nil
}

// run is Run before its error is redacted: the secret values it has read
// reach Run through redactp, also when it fails part-way.
func (l *Login) run(ctx context.Context, s *config.Service, redactp *[]string) (*Result, error) {
	if s.Auth.Kind != config.ServiceAuthCookie {
		return nil, fmt.Errorf("the service %s uses a header credential, which needs no login: store it with 'devc secret set %s %s'", s.Name, s.Realm, s.Auth.ValueSecret)
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	up, err := s.UpstreamURL()
	if err != nil {
		return nil, err
	}
	stateDir := l.Paths.ServiceStateDir(s.Realm)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	unlock, err := flock.Acquire(ctx, l.Paths.ServiceLockFile(s.Realm, s.Name), flock.Options{Timeout: loginLockWait})
	if err != nil {
		return nil, fmt.Errorf("waiting for another login of %s: %w", s.Name, err)
	}
	defer unlock()

	values := map[string][]byte{}
	var redact []string
	defer func() { *redactp = redact }()
	for _, n := range s.Auth.Secrets {
		v, err := l.Store.Get(s.Realm, n)
		if errors.Is(err, secrets.ErrNotFound) {
			return nil, fmt.Errorf("the login of %s needs the secret %s: run 'devc secret set %s %s'", s.Name, n, s.Realm, n)
		}
		if err != nil {
			return nil, fmt.Errorf("reading the secret %s: %w", n, err)
		}
		values[n] = v
		// The value as stored, without its line ending, and without any
		// surrounding white space, which a login script may well strip.
		redact = append(redact, string(v), string(secrets.TrimValue(v)), strings.TrimSpace(string(v)))
	}
	if err := l.Paths.Ensure(l.Paths.TmpDir()); err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp(l.Paths.TmpDir(), "service-login-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	if err := os.Chmod(scratch, 0o700); err != nil {
		return nil, err
	}
	if err := copyLoginFiles(s.Dir, scratch); err != nil {
		return nil, err
	}
	sdir := filepath.Join(scratch, "secrets")
	if err := os.Mkdir(sdir, 0o700); err != nil {
		return nil, err
	}
	for n, v := range values {
		if err := os.WriteFile(filepath.Join(sdir, n), v, 0o600); err != nil {
			return nil, err
		}
	}

	image, tmpfs := s.Auth.Image, []string{"/tmp"}
	if image == "" {
		image, tmpfs = toolbox.InstallerImage, toolbox.InstallerTmpfs
		out := l.Out
		if out == nil {
			out = io.Discard
		}
		if err := toolbox.EnsureInstallerImage(ctx, l.Engine, l.ContextDir, out); err != nil {
			return nil, fmt.Errorf("preparing the installer image: %w", err)
		}
	}
	stdout := &cappedBuffer{max: MaxLoginOutput}
	stderr := &cappedBuffer{max: maxLoginStderr}
	env := []string{"HOME=/tmp", "DEVC_SERVICE=" + s.Name, "DEVC_SERVICE_UPSTREAM=" + up.String(), "DEVC_SECRETS_DIR=" + LoginSecretsDir}
	if s.Auth.LoginPath != "" {
		env = append(env, "DEVC_LOGIN_PATH="+s.Auth.LoginPath)
	}
	code, err := l.Engine.RunStream(ctx, engine.RunOptions{
		Image: image, SecurityOpts: l.SecurityOpts, CapDropAll: true,
		Labels: map[string]string{toolbox.LabelRole: toolbox.RoleAux, "devc.aux": "service-login"},
		Mounts: []engine.RunMount{{Source: scratch, Target: LoginDir}}, Tmpfs: tmpfs, Env: env, Workdir: LoginDir,
		Cmd: s.Auth.Login, Stdout: stdout, Stderr: stderr, Timeout: LoginTimeout,
	})
	errText := cleanStderr(stderr.buf.String(), redact)
	if err != nil {
		return nil, fmt.Errorf("running the login command of %s: %w", s.Name, err)
	}
	if code != 0 {
		return nil, fmt.Errorf("the login command of %s exited with status %d%s", s.Name, code, tail(errText))
	}
	if stdout.over {
		return nil, fmt.Errorf("the login command of %s printed more than %d bytes", s.Name, MaxLoginOutput)
	}
	cookies, err := ParseLoginOutput(stdout.buf.Bytes(), up.Hostname(), now())
	if err != nil {
		return nil, fmt.Errorf("%s: %w%s", s.Name, err, tail(errText))
	}
	jar := NewJar(s.Name, cookies, now())
	b, err := jar.Marshal()
	if err != nil {
		return nil, err
	}
	if err := l.Store.Set(s.Realm, config.JarSecret(s.Name), b); err != nil {
		return nil, fmt.Errorf("storing the cookies of %s: %w", s.Name, err)
	}
	stateFile := l.Paths.ServiceStateFile(s.Realm, s.Name)
	if err := WriteState(stateFile, State{V: 1}); err != nil {
		return nil, fmt.Errorf("recording the login of %s: %w", s.Name, err)
	}
	return &Result{Jar: jar, Stderr: cleanStderr(errText, jar.Values())}, nil
}

// copyLoginFiles copies the regular files of the service's directory,
// except service.toml and hidden files, into the scratch directory. The
// configuration directory is trusted; the limits only catch mistakes.
func copyLoginFiles(src, dst string) error {
	des, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	n := 0
	for _, de := range des {
		name := de.Name()
		if name == "service.toml" || name == "secrets" || strings.HasPrefix(name, ".") {
			continue
		}
		fi, err := os.Stat(filepath.Join(src, name))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if n++; n > maxLoginFiles {
			return fmt.Errorf("%s holds more than %d files", src, maxLoginFiles)
		}
		if fi.Size() > maxLoginFile {
			return fmt.Errorf("%s is larger than %d bytes", filepath.Join(src, name), maxLoginFile)
		}
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if fi.Mode()&0o100 != 0 {
			mode = 0o700
		}
		if err := os.WriteFile(filepath.Join(dst, name), b, mode); err != nil {
			return err
		}
	}
	return nil
}

// cleanStderr removes secret values (every one, longest first, so that a
// value inside another goes too) and control characters from the command's
// standard error or a message derived from its output.
func cleanStderr(s string, redact []string) string {
	vals := slices.Clone(redact)
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		if v != "" {
			s = strings.ReplaceAll(s, v, Redacted)
		}
	}
	return proto.PrintableText(s)
}

// tail is the end of the command's standard error for a message.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return ":\n" + strings.Join(lines, "\n")
}
