package cli

// Service logins (work order C18): devc service ls, login, status <realm>
// <name> and log. The other subcommands of `devc service` manage the
// systemd units (servicecmds.go); `devc service status` without arguments
// still reports those.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/svclogin"
)

// serviceStatus is what `devc service ls` and `status` report about one
// service.
type serviceStatus struct {
	Realm    string   `json:"realm"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Port     int      `json:"port"`
	Upstream string   `json:"upstream"`
	Projects []string `json:"projects"`
	Rules    []string `json:"rules"`
	// Login is "ok", "needs-login", "no-secret", "invalid-secret" (too
	// short, or not one line) or "invalid"; Detail says
	// more.
	Login  string `json:"login"`
	Detail string `json:"detail,omitempty"`
	// Cookie services.
	LoginAt   *time.Time `json:"login_at,omitempty"`
	ValidTill *time.Time `json:"valid_until,omitempty"`
	Refresh   string     `json:"refresh,omitempty"`
	Auto      bool       `json:"auto,omitempty"`
	AutoError string     `json:"auto_error,omitempty"`
	Problems  []string   `json:"problems,omitempty"`
}

// statusOf describes a service from the configuration, the secret store
// and the machine's login state.
func statusOf(c *config.Config, s *config.Service, store secrets.Store, now time.Time) serviceStatus {
	st := serviceStatus{Realm: s.Realm, Name: s.Name, Kind: s.Auth.Kind, Port: s.Port, Upstream: s.Upstream, Projects: []string{}, Rules: []string{}}
	for _, ref := range c.GrantedTo(s) {
		st.Projects = append(st.Projects, ref.String())
	}
	for _, r := range s.Allow {
		st.Rules = append(st.Rules, strings.Join(r.Methods, ",")+" "+r.Path)
	}
	for _, p := range c.ServiceProblems(s.Realm, s.Name) {
		st.Problems = append(st.Problems, p.Key+": "+p.Message)
	}
	if len(st.Problems) > 0 {
		st.Login, st.Detail = "invalid", "the definition has problems; run 'devc config check'"
		return st
	}
	switch s.Auth.Kind {
	case config.ServiceAuthHeader:
		v, err := store.Get(s.Realm, s.Auth.ValueSecret)
		switch {
		case err != nil:
			st.Login, st.Detail = "no-secret", fmt.Sprintf("the secret %s is not set ('devc secret set %s %s')", s.Auth.ValueSecret, s.Realm, s.Auth.ValueSecret)
		default:
			if _, err := svclogin.HeaderCredential(s.Auth.Header, v); err != nil {
				st.Login, st.Detail = "invalid-secret", fmt.Sprintf("the secret %s cannot be used: %s ('devc secret set %s %s')", s.Auth.ValueSecret, err, s.Realm, s.Auth.ValueSecret)
			} else {
				st.Login, st.Detail = "ok", "header "+s.Auth.Header+" from the secret "+s.Auth.ValueSecret
			}
		}
	case config.ServiceAuthCookie:
		st.Refresh, st.Auto = s.Auth.Refresh, s.Auth.Auto
		up, _ := s.UpstreamURL()
		host := ""
		if up != nil {
			host = up.Hostname()
		}
		var jar *svclogin.Jar
		b, err := store.Get(s.Realm, config.JarSecret(s.Name))
		switch {
		case errors.Is(err, secrets.ErrNotFound):
		case err != nil:
			st.Login, st.Detail = "needs-login", "the cookie jar cannot be read: "+err.Error()
			return st
		default:
			if jar, err = svclogin.ParseJar(b, s.Name); err != nil {
				st.Login, st.Detail = "needs-login", err.Error()
				return st
			}
			at := jar.LoginAt
			st.LoginAt = &at
			till := jar.LoginAt.Add(s.RefreshDuration())
			if e := jar.Expires(host); !e.IsZero() && e.Before(till) {
				till = e
			}
			st.ValidTill = &till
		}
		state, _ := svclogin.ReadState(c.Paths.ServiceStateFile(s.Realm, s.Name))
		st.AutoError = state.AutoError
		if p := jar.Problem(s.RefreshDuration(), host, now); p != "" {
			st.Login, st.Detail = "needs-login", p
		} else if need, reason := state.Current(jar); need {
			st.Login, st.Detail = "needs-login", reason
		} else {
			st.Login = "ok"
		}
	}
	return st
}

func newServiceLoginCmds(a *app) []*cobra.Command {
	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls [realm]",
		Short: "List the services boxes can use through the daemon, and their login state",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			realm := ""
			if len(args) == 1 {
				if err := a.realmArg(c, args[0]); err != nil {
					return err
				}
				realm = args[0]
			}
			store := secrets.NewFileStore(c.Paths)
			rows := []serviceStatus{}
			for _, s := range c.Services(realm) {
				rows = append(rows, statusOf(c, s, store, time.Now()))
			}
			if lsJSON {
				return writeJSON(a.stdout, rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(a.stderr, "devc: no services (define one in realms/<realm>/services/<name>/service.toml)")
				return nil
			}
			tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVICE\tKIND\tPORT\tUPSTREAM\tLOGIN\tPROJECTS")
			for _, r := range rows {
				login := r.Login
				if r.Login != "ok" {
					login += " (" + r.Detail + ")"
				} else if r.ValidTill != nil {
					login += " until " + r.ValidTill.Local().Format("2006-01-02 15:04")
				}
				projects := strings.Join(r.Projects, ",")
				if projects == "" {
					projects = "-"
				}
				fmt.Fprintf(tw, "%s/%s\t%s\t%d\t%s\t%s\t%s\n", r.Realm, r.Name, r.Kind, r.Port, r.Upstream, proto.Printable(login), projects)
			}
			return tw.Flush()
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	var quiet bool
	login := &cobra.Command{
		Use:   "login <realm> <name>",
		Short: "Log in to a cookie service and store its cookies for the daemon",
		Long: "Run the service's login command (auth.login) in a throwaway container: the image auth.image (default the " +
			"installer image), the network on, nothing mounted but a scratch directory at " + svclogin.LoginDir + " that holds the other files " +
			"of the service's directory and the secrets of auth.secrets in " + svclogin.LoginSecretsDir + ". The command prints the cookies " +
			"as a JSON array (name, value, domain, path, expiry) on standard output; devc stores them as the realm secret " +
			config.JarSecretPrefix + "<name> with the time of the login. The cookies never enter a box.",
		Args: exactArgs(2, "devc service login <realm> <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.load()
			if err != nil {
				return err
			}
			if s, err := a.serviceArg(c, args[0], args[1]); err != nil {
				return err
			} else if s.Auth.Kind != config.ServiceAuthCookie {
				return fmt.Errorf("the service %s/%s uses a header credential, which needs no login: store it with 'devc secret set %s %s'", s.Realm, s.Name, s.Realm, s.Auth.ValueSecret)
			}
			// The toolbox's seccomp profile confines the login container.
			e, err := a.toolEnv()
			if err != nil {
				return err
			}
			s, err := a.serviceArg(e.cfg, args[0], args[1])
			if err != nil {
				return err
			}
			if probs := e.cfg.ServiceProblems(s.Realm, s.Name); len(probs) > 0 {
				return fmt.Errorf("the definition of %s/%s has problems; run 'devc config check'", s.Realm, s.Name)
			}
			l := &svclogin.Login{Engine: e.eng, Store: secrets.NewFileStore(e.cfg.Paths), Paths: e.cfg.Paths,
				SecurityOpts: e.auxSecurityOpts(), ContextDir: filepath.Join(e.cfg.Paths.CacheDir(), "aux", "installer"), Out: a.stderr}
			ctx, cancel := context.WithTimeout(cmd.Context(), svclogin.LoginTimeout+2*time.Minute)
			defer cancel()
			res, err := l.Run(ctx, s)
			if err != nil {
				return err
			}
			if !quiet {
				if t := strings.TrimSpace(res.Stderr); t != "" {
					fmt.Fprintln(a.stderr, t)
				}
				st := statusOf(e.cfg, s, secrets.NewFileStore(e.cfg.Paths), time.Now())
				till := ""
				if st.ValidTill != nil {
					till = ", valid until " + st.ValidTill.Local().Format("2006-01-02 15:04")
				}
				fmt.Fprintf(a.stdout, "logged in to %s/%s: %d cookie(s)%s\n", s.Realm, s.Name, len(res.Jar.Cookies), till)
			}
			return nil
		},
	}
	login.Flags().BoolVar(&quiet, "quiet", false, "print nothing on success")

	var logJSON bool
	var logN int
	logCmd := &cobra.Command{
		Use:   "log <realm> <name> [-n N]",
		Short: "Show the requests boxes made to a service (time, box, method, path, status)",
		Args:  exactArgs(2, "devc service log <realm> <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			if err := a.realmArg(c, args[0]); err != nil {
				return err
			}
			if err := names.Check(names.KindService, args[1]); err != nil {
				return &usageError{msg: err.Error()}
			}
			list, err := svclogin.ReadLog(c.Paths.ServiceLogFile(args[0], args[1]))
			if err != nil {
				return err
			}
			if logN > 0 && len(list) > logN {
				list = list[len(list)-logN:]
			}
			if logJSON {
				if list == nil {
					list = []svclogin.LogEntry{}
				}
				return writeJSON(a.stdout, list)
			}
			return printServiceLog(a.stdout, list)
		},
	}
	logCmd.Flags().BoolVar(&logJSON, "json", false, "print JSON")
	logCmd.Flags().IntVarP(&logN, "lines", "n", 50, "show the last N requests (0: all)")
	return []*cobra.Command{ls, login, logCmd}
}

func printServiceLog(w io.Writer, list []svclogin.LogEntry) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tBOX\tMETHOD\tPATH\tSTATUS\tNOTE")
	for _, e := range list {
		m := e.Method
		if m == "" {
			m = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", e.Time.Local().Format("2006-01-02 15:04:05"), proto.Printable(e.Box), proto.Printable(m),
			proto.Printable(e.Path), e.Status, proto.Printable(proto.Truncate(e.Reason, 80)))
	}
	return tw.Flush()
}

// serviceArg finds a service named on the command line.
func (a *app) serviceArg(c *config.Config, realm, name string) (*config.Service, error) {
	if err := a.realmArg(c, realm); err != nil {
		return nil, err
	}
	if err := names.Check(names.KindService, name); err != nil {
		return nil, &usageError{msg: err.Error()}
	}
	s, ok := c.Service(realm, name)
	if !ok {
		if len(c.ServiceProblems(realm, name)) > 0 {
			return nil, fmt.Errorf("the definition of %s/%s failed to load; run 'devc config check'", realm, name)
		}
		return nil, fmt.Errorf("no service %q in realm %s ('devc service ls %s')", name, realm, realm)
	}
	return s, nil
}

// serviceStatusCmd prints the status of one service (`devc service status
// <realm> <name>`).
func (a *app) serviceStatusCmd(args []string, asJSON bool) error {
	c, probs, err := a.load()
	if err != nil {
		return err
	}
	a.warnProblems(probs)
	if err := a.realmArg(c, args[0]); err != nil {
		return err
	}
	if err := names.Check(names.KindService, args[1]); err != nil {
		return &usageError{msg: err.Error()}
	}
	s, ok := c.Service(args[0], args[1])
	if !ok {
		if _, err := a.serviceArg(c, args[0], args[1]); err != nil {
			return err
		}
	}
	st := statusOf(c, s, secrets.NewFileStore(c.Paths), time.Now())
	if asJSON {
		if err := writeJSON(a.stdout, st); err != nil {
			return err
		}
		if st.Login != "ok" {
			return exitCode(1)
		}
		return nil
	}
	w := a.stdout
	fmt.Fprintf(w, "service:   %s/%s (%s)\n", st.Realm, st.Name, st.Kind)
	fmt.Fprintf(w, "upstream:  %s\n", st.Upstream)
	fmt.Fprintf(w, "in boxes:  http://127.0.0.1:%d/\n", st.Port)
	fmt.Fprintf(w, "allows:    %s\n", strings.Join(st.Rules, "; "))
	projects := strings.Join(st.Projects, ", ")
	if projects == "" {
		projects = "none (grants.services in project.toml)"
	}
	fmt.Fprintf(w, "granted:   %s\n", projects)
	login := st.Login
	if st.Detail != "" {
		login += ": " + st.Detail
	}
	fmt.Fprintf(w, "login:     %s\n", proto.Printable(login))
	if st.LoginAt != nil {
		fmt.Fprintf(w, "logged in: %s\n", st.LoginAt.Local().Format(time.RFC3339))
	}
	if st.ValidTill != nil {
		fmt.Fprintf(w, "valid:     until %s (refresh %s)\n", st.ValidTill.Local().Format(time.RFC3339), st.Refresh)
	}
	if st.Kind == config.ServiceAuthCookie {
		auto := "no (run 'devc service login " + st.Realm + " " + st.Name + "' when it needs one)"
		if st.Auto {
			auto = "yes, by the daemon"
		}
		fmt.Fprintf(w, "auto:      %s\n", auto)
	}
	if st.AutoError != "" {
		fmt.Fprintf(w, "last automatic login failed: %s\n", proto.Printable(st.AutoError))
	}
	for _, p := range st.Problems {
		fmt.Fprintf(w, "problem:   %s\n", proto.Printable(p))
	}
	if st.Login != "ok" {
		return exitCode(1)
	}
	return nil
}
