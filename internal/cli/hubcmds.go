package cli

// Hub commands (work order C11a): the hub server's own commands (`devc hub
// init`, `ca`, `serve`, working on DEVC_HUB_DIR) and the machine's side
// (`devc join`, `devc leave`, `devc hub status`).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/hub"
	"github.com/mb706/devc/internal/hubnode"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// hubDir resolves the hub directory: --hub-dir, $DEVC_HUB_DIR, or
// ~/.local/share/devc-hub.
func (a *app) hubDir(flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	return hub.ResolveDir(a.getenv, a.getenv("HOME"))
}

func newHubCmd(a *app) *cobra.Command {
	var dirFlag string

	var publicName string
	initCmd := &cobra.Command{
		Use:   "init [--public-name host[:port]]",
		Short: "Create the hub directory, its CA and database",
		Long: "Create the hub directory ($DEVC_HUB_DIR, default ~/.local/share/devc-hub) with a private CA (ECDSA P-256, " +
			"ten years) and the database. With --public-name it also writes the server key, a certificate request for a " +
			"public CA (server/server.csr) and a server certificate from the devc CA (server/server.crt) for that name.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := a.hubDir(dirFlag)
			if err != nil {
				return err
			}
			defer syscall.Umask(syscall.Umask(0o077))
			if err := hub.Init(dir, publicName); err != nil {
				return err
			}
			l := hub.Layout{Dir: dir}
			fmt.Fprintf(a.stdout, "created the hub in %s\n  CA certificate: %s (give it to proxies and to machines with --ca)\n", dir, l.CACert())
			if publicName != "" {
				fmt.Fprintf(a.stdout, "  server certificate for %s: %s (key %s, request %s)\n", publicName, l.ServerCert(), l.ServerKey(), l.ServerCSR())
			}
			fmt.Fprintln(a.stdout, "next: 'devc hub ca issue machine <name>' and 'devc hub ca issue device <name>', then 'devc hub serve'")
			return nil
		},
	}
	initCmd.Flags().StringVar(&publicName, "public-name", "", "host[:port] clients use to reach the hub")

	var outDir string
	var legacy bool
	issue := &cobra.Command{
		Use:   "issue machine|device|server <name> [--out dir]",
		Short: "Issue a client certificate (or the hub's server certificate)",
		Long: "Issue a key and a one-year certificate from the hub's CA. 'machine' certificates let a machine daemon " +
			"connect ('devc join'); 'device' certificates let a browser or phone use the API, and come with a PKCS#12 " +
			"bundle whose passphrase is printed once. Files go to --out (default: the current directory) and are never " +
			"overwritten. 'server <host>' replaces the hub's own server certificate in the hub directory.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 2 {
				return usageErrorf("usage: devc hub ca issue machine|device|server <name>")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			role, name := args[0], args[1]
			switch {
			case role != hub.RoleMachine && role != hub.RoleDevice && role != "server":
				return usageErrorf("the role is machine, device or server, not %q", role)
			case role != "server" && !names.Valid(name):
				return usageErrorf("%q is not a valid name (a-z, 0-9, '.', '_', '-'; up to 63 characters)", name)
			}
			dir, err := a.hubDir(dirFlag)
			if err != nil {
				return err
			}
			defer syscall.Umask(syscall.Umask(0o077))
			l := hub.Layout{Dir: dir}
			ca, err := hub.LoadCA(l)
			if err != nil {
				return err
			}
			if role == "server" {
				if err := ca.IssueServer(l, name); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "issued the server certificate for %s: %s (key %s, request %s)\n", name, l.ServerCert(), l.ServerKey(), l.ServerCSR())
				return nil
			}
			out := outDir
			if out == "" {
				if out, err = a.getwd(); err != nil {
					return err
				}
			}
			is, err := ca.IssueClient(role, name, legacy)
			if err != nil {
				return err
			}
			files := []struct {
				path string
				data []byte
				mode os.FileMode
			}{
				{filepath.Join(out, name+".crt"), is.CertPEM, 0o644},
				{filepath.Join(out, name+".key"), is.KeyPEM, 0o600},
			}
			if is.P12 != nil {
				files = append(files, struct {
					path string
					data []byte
					mode os.FileMode
				}{filepath.Join(out, name+".p12"), is.P12, 0o600})
			}
			for _, f := range files {
				if _, err := os.Lstat(f.path); err == nil {
					return fmt.Errorf("%s exists already: remove it or choose another --out", f.path)
				}
			}
			var written []string
			for _, f := range files {
				if err := writeExclusive(f.path, f.data, f.mode); err != nil {
					for _, w := range written {
						os.Remove(w)
					}
					return err
				}
				written = append(written, f.path)
			}
			st, err := hub.OpenStore(l.DB())
			if err != nil {
				return err
			}
			defer st.Close()
			rec := hub.CertRecord{Fingerprint: is.Fingerprint, Role: role, Name: name, Serial: is.Cert.SerialNumber.Text(16), Issued: is.Cert.NotBefore, Expires: is.Cert.NotAfter}
			if err := st.AddCert(rec); err != nil {
				for _, w := range written {
					os.Remove(w)
				}
				return fmt.Errorf("recording the certificate: %w", err)
			}
			fmt.Fprintf(a.stdout, "issued a %s certificate for %s (fingerprint %s, expires %s)\n", role, name, is.Fingerprint[:16], is.Cert.NotAfter.Format("2006-01-02"))
			fmt.Fprintf(a.stdout, "  certificate: %s\n  key:         %s\n", files[0].path, files[1].path)
			if is.P12 != nil {
				fmt.Fprintf(a.stdout, "  bundle:      %s\n  passphrase:  %s (shown once; needed to import the bundle)\n", files[2].path, is.Passphrase)
			} else {
				fmt.Fprintf(a.stdout, "on the machine: devc join https://<hub> --cert %s.crt --key %s.key [--ca ca.crt]\n", name, name)
			}
			return nil
		},
	}
	issue.Flags().StringVar(&outDir, "out", "", "directory for the files (default: the current directory)")
	issue.Flags().BoolVar(&legacy, "legacy-p12", false, "encrypt the PKCS#12 bundle with the older algorithms some phones need")

	var lsJSON bool
	lsCmd := &cobra.Command{
		Use:   "ls [--json]",
		Short: "List the issued certificates",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := a.hubDir(dirFlag)
			if err != nil {
				return err
			}
			st, err := hub.OpenStore(hub.Layout{Dir: dir}.DB())
			if err != nil {
				return err
			}
			defer st.Close()
			certs, err := st.Certs()
			if err != nil {
				return err
			}
			if lsJSON {
				if certs == nil {
					certs = []hub.CertRecord{}
				}
				return writeJSON(a.stdout, certs)
			}
			tw := table(a.stdout, "ROLE", "NAME", "FINGERPRINT", "ISSUED", "EXPIRES", "STATE")
			now := time.Now()
			for _, c := range certs {
				state := "valid"
				switch {
				case c.Revoked != nil:
					state = "revoked " + c.Revoked.Local().Format("2006-01-02 15:04")
				case now.After(c.Expires):
					state = "expired"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Role, c.Name, c.Fingerprint[:16], c.Issued.Local().Format("2006-01-02"), c.Expires.Local().Format("2006-01-02"), state)
			}
			return tw.Flush()
		},
	}
	lsCmd.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	var revokeRole string
	revoke := &cobra.Command{
		Use:   "revoke <name|fingerprint> [--role machine|device]",
		Short: "Revoke certificates; connections using them end at once",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageErrorf("usage: devc hub ca revoke <name|fingerprint>")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if revokeRole != "" && revokeRole != hub.RoleMachine && revokeRole != hub.RoleDevice {
				return usageErrorf("--role is machine or device")
			}
			dir, err := a.hubDir(dirFlag)
			if err != nil {
				return err
			}
			st, err := hub.OpenStore(hub.Layout{Dir: dir}.DB())
			if err != nil {
				return err
			}
			defer st.Close()
			hit, err := st.Revoke(args[0], revokeRole)
			if err != nil {
				return err
			}
			if len(hit) == 0 {
				return fmt.Errorf("no valid certificate matches %q ('devc hub ca ls')", args[0])
			}
			for _, c := range hit {
				fmt.Fprintf(a.stdout, "revoked the %s certificate of %s (%s)\n", c.Role, c.Name, c.Fingerprint[:16])
			}
			return nil
		},
	}
	revoke.Flags().StringVar(&revokeRole, "role", "", "only certificates of this role")

	ca := group("ca", "Manage the hub's certificates", false, issue, lsCmd, revoke)

	var listen, tlsCert, tlsKey, servePublic, level string
	var behindProxy bool
	serve := &cobra.Command{
		Use:   "serve --listen addr [--tls-cert file --tls-key file | --behind-proxy]",
		Short: "Run the hub server in the foreground",
		Long: "Run the hub. Direct TLS: the hub terminates TLS itself and requires a client certificate from its CA " +
			"(default certificate: the one 'devc hub init --public-name' wrote). Behind a reverse proxy: --behind-proxy " +
			"with a loopback --listen address; the proxy verifies the client certificate and passes it in the " +
			"X-Devc-Client-Cert header. See docs/hub-deployment.md.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lvl, err := parseLevel(level)
			if err != nil {
				return err
			}
			if behindProxy && (tlsCert != "" || tlsKey != "") {
				return usageErrorf("--behind-proxy and --tls-cert/--tls-key exclude each other")
			}
			dir, err := a.hubDir(dirFlag)
			if err != nil {
				return err
			}
			for _, f := range []*string{&tlsCert, &tlsKey} {
				if *f != "" {
					if *f, err = filepath.Abs(*f); err != nil {
						return err
					}
				}
			}
			defer syscall.Umask(syscall.Umask(0o077))
			if err := os.Chdir("/"); err != nil {
				return err
			}
			log := slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: lvl}))
			s, err := hub.New(hub.Options{Dir: dir, Listen: listen, TLSCert: tlsCert, TLSKey: tlsKey, BehindProxy: behindProxy, PublicName: servePublic, Log: log})
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			return s.Run(ctx)
		},
	}
	serve.Flags().StringVar(&listen, "listen", "", "address to listen on (behind a proxy: 127.0.0.1:<port>)")
	serve.Flags().StringVar(&tlsCert, "tls-cert", "", "server certificate (PEM, with chain)")
	serve.Flags().StringVar(&tlsKey, "tls-key", "", "server key (PEM)")
	serve.Flags().BoolVar(&behindProxy, "behind-proxy", false, "plain HTTP on loopback behind a reverse proxy that verifies client certificates")
	serve.Flags().StringVar(&servePublic, "public-name", "", "host[:port] the Host header must name (default: from 'devc hub init')")
	serve.Flags().StringVar(&level, "log-level", "info", "debug, info, warn or error")

	var statusJSON bool
	status := &cobra.Command{
		Use:   "status [--json]",
		Short: "Show this machine's hubs and whether the daemon is connected to them",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			var res proto.HubsStatusResult
			cl, derr := a.daemonClient(ctx, c, nil)
			if derr == nil {
				defer cl.Close()
				if err := cl.Call(ctx, proto.MethodHubsStatus, nil, &res); err != nil {
					return fmt.Errorf("asking the daemon: %w", err)
				}
			} else {
				for _, h := range c.Global.Hubs {
					u, err := hubnode.NormalizeURL(h.URL)
					if err != nil {
						continue
					}
					st := proto.HubStatus{URL: u, State: "daemon not running"}
					if _, err := hubnode.Load(c.Paths, u); errors.Is(err, hubnode.ErrNoCreds) {
						st.State = proto.HubNoCreds
					}
					res.Hubs = append(res.Hubs, st)
				}
				if res.Hubs == nil {
					res.Hubs = []proto.HubStatus{}
				}
			}
			if statusJSON {
				if err := writeJSON(a.stdout, res); err != nil {
					return err
				}
			} else if len(res.Hubs) == 0 {
				fmt.Fprintln(a.stdout, "no hubs listed ('devc join <hub-url> --cert <file> --key <file>')")
			} else {
				tw := table(a.stdout, "HUB", "STATE", "NOTIFIER", "MACHINE", "SINCE", "DETAIL")
				for _, h := range res.Hubs {
					since := ""
					if h.Since != nil {
						since = h.Since.Local().Format("2006-01-02 15:04:05")
					}
					notifier := "no"
					if h.Notifier {
						notifier = "yes"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", h.URL, h.State, notifier, h.Machine, since, oneLine(h.Detail, 80))
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			if derr != nil {
				if !statusJSON {
					fmt.Fprintln(a.stderr, "devc: the daemon is not running; hubs are connected only while it runs")
				}
				return exitCode(1)
			}
			return nil
		},
	}
	status.Flags().BoolVar(&statusJSON, "json", false, "print JSON")

	cmd := group("hub", "Run a hub, or show this machine's hubs", false, initCmd, ca, serve, status)
	cmd.PersistentFlags().StringVar(&dirFlag, "hub-dir", "", "hub directory (default $DEVC_HUB_DIR or ~/.local/share/devc-hub)")
	return cmd
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	// The mode as asked, whatever the umask: certificates are public.
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

func newJoinCmd(a *app) *cobra.Command {
	var certFile, keyFile, caFile string
	cmd := &cobra.Command{
		Use:   "join <hub-url> --cert <file> --key <file> [--ca <file>]",
		Short: "Report to a hub: store its credentials and add it to the hub list",
		Long: "Store the machine certificate and key a hub issued ('devc hub ca issue machine') under " +
			"durable/node/hubs/, and add the hub to the [[hubs]] list in config.toml. --ca is the CA that signed the " +
			"hub's server certificate, when it is not a public one. The daemon connects within seconds; " +
			"'devc hub status' shows the connection.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageErrorf("usage: devc join <hub-url> --cert <file> --key <file> [--ca <file>]")
			}
			if certFile == "" || keyFile == "" {
				return usageErrorf("--cert and --key are required")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			u, err := hubnode.NormalizeURL(args[0])
			if err != nil {
				return usageErrorf("%s", err.Error())
			}
			certPEM, err := os.ReadFile(certFile)
			if err != nil {
				return err
			}
			keyPEM, err := os.ReadFile(keyFile)
			if err != nil {
				return err
			}
			var caPEM []byte
			if caFile != "" {
				if caPEM, err = os.ReadFile(caFile); err != nil {
					return err
				}
			}
			leaf, err := hubnode.Check(certPEM, keyPEM, caPEM)
			if err != nil {
				return err
			}
			if role := hub.RoleOf(leaf); role != hub.RoleMachine {
				return fmt.Errorf("this is a %q certificate; a machine needs one from 'devc hub ca issue machine <name>'", role)
			}
			c, _, err := a.load()
			if err != nil {
				return err
			}
			if err := hubnode.Save(c.Paths, u, certPEM, keyPEM, caPEM); err != nil {
				return err
			}
			added, err := hubnode.AddToConfig(c.Paths.ConfigFile(), u)
			if err != nil {
				return err
			}
			if added {
				fmt.Fprintf(a.stdout, "joined %s as %s (added to %s)\n", u, leaf.Subject.CommonName, c.Paths.ConfigFile())
			} else {
				fmt.Fprintf(a.stdout, "joined %s as %s (credentials replaced)\n", u, leaf.Subject.CommonName)
			}
			fmt.Fprintln(a.stdout, "the daemon connects within seconds; check with 'devc hub status'")
			return nil
		},
	}
	cmd.Flags().StringVar(&certFile, "cert", "", "machine certificate (PEM)")
	cmd.Flags().StringVar(&keyFile, "key", "", "its key (PEM)")
	cmd.Flags().StringVar(&caFile, "ca", "", "CA of the hub's server certificate (default: the system's roots)")
	return cmd
}

func newLeaveCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "leave <hub-url>",
		Short: "Stop reporting to a hub: remove it from the hub list and delete its credentials",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageErrorf("usage: devc leave <hub-url>")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			u, err := hubnode.NormalizeURL(args[0])
			if err != nil {
				return usageErrorf("%s", err.Error())
			}
			c, _, err := a.load()
			if err != nil {
				return err
			}
			listed, err := hubnode.RemoveFromConfig(c.Paths.ConfigFile(), u)
			if err != nil {
				return err
			}
			had, err := hubnode.Remove(c.Paths, u)
			if err != nil {
				return err
			}
			if !listed && !had {
				return fmt.Errorf("this machine has not joined %s", u)
			}
			fmt.Fprintf(a.stdout, "left %s\n", u)
			return nil
		},
	}
}
