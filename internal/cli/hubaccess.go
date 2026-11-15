package cli

// Hub access commands (work order C11b): the user's password and TOTP,
// login sessions, the device and machine registry, enrolment tokens and
// the doctor, all working on the hub directory; and `devc join --token`
// on a machine.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/hub"
	"github.com/mb706/devc/internal/names"
)

// openHub opens the store of an initialised hub directory.
func (a *app) openHub(dirFlag string) (hub.Layout, *hub.Store, error) {
	dir, err := a.hubDir(dirFlag)
	if err != nil {
		return hub.Layout{}, nil, err
	}
	l := hub.Layout{Dir: dir}
	if _, err := os.Stat(l.CACert()); err != nil {
		return l, nil, fmt.Errorf("%s is not a hub directory ('devc hub init')", dir)
	}
	st, err := hub.OpenStore(l.DB())
	return l, st, err
}

// readLine reads one line from a non-terminal stdin.
func (a *app) readLine(max int) (string, error) {
	r := bufio.NewReaderSize(io.LimitReader(a.stdinReader(), int64(max)+2), max+2)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if len(line) > max+1 {
		return "", fmt.Errorf("the input is longer than %d bytes", max)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readPassword reads a new password: twice from a hidden prompt, or one
// line from stdin when it is not a terminal.
func (a *app) readPassword() (string, error) {
	var pw string
	if a.isTerminal() {
		p1, err := readHidden(int(os.Stdin.Fd()), "new hub password: ", a.stderr)
		if err != nil {
			return "", err
		}
		p2, err := readHidden(int(os.Stdin.Fd()), "again: ", a.stderr)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(p1, p2) {
			return "", errors.New("the two passwords differ")
		}
		pw = string(p1)
	} else {
		var err error
		if pw, err = a.readLine(hub.MaxPasswordLength); err != nil {
			return "", err
		}
	}
	if n := len([]rune(pw)); n < hub.MinPasswordLength {
		return "", fmt.Errorf("the password is too short: at least %d characters", hub.MinPasswordLength)
	}
	if len(pw) > hub.MaxPasswordLength {
		return "", fmt.Errorf("the password is longer than %d bytes", hub.MaxPasswordLength)
	}
	for _, r := range pw {
		if unicode.IsControl(r) {
			return "", errors.New("the password holds a control character")
		}
	}
	return pw, nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func certState(c hub.CertRecord, now time.Time) string {
	switch {
	case c.Revoked != nil:
		return "revoked " + fmtTime(*c.Revoked)
	case now.After(c.Expires):
		return "expired"
	}
	return "valid"
}

// newHubAccessCmds returns the commands of C11b below `devc hub`.
func newHubAccessCmds(a *app, dirFlag *string) []*cobra.Command {
	// --- user ---
	setPassword := &cobra.Command{
		Use:   "set-password",
		Short: "Set the password of the hub's user (from a hidden prompt, or one line on stdin)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, st, err := a.openHub(*dirFlag)
			if err != nil {
				return err
			}
			defer st.Close()
			pw, err := a.readPassword()
			if err != nil {
				return err
			}
			h, err := hub.HashPassword(pw)
			if err != nil {
				return err
			}
			if err := st.SetPassword(h); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "password set (existing login sessions stay; 'devc hub sessions revoke --all' ends them)")
			return nil
		},
	}
	totpEnable := &cobra.Command{
		Use:   "enable",
		Short: "Require a TOTP code at login; prints the secret once",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, st, err := a.openHub(*dirFlag)
			if err != nil {
				return err
			}
			defer st.Close()
			secret, err := hub.NewTOTPSecret()
			if err != nil {
				return err
			}
			if err := st.SetTOTP(secret); err != nil {
				return err
			}
			account, _ := st.Meta("public_name")
			if account == "" {
				account = "hub"
			}
			fmt.Fprintf(a.stdout, "TOTP enabled; logins now need a code. Add this to an authenticator app (shown once):\n  secret: %s\n  uri:    %s\n", secret, hub.TOTPURI(secret, account))
			return nil
		},
	}
	totpDisable := &cobra.Command{
		Use:   "disable",
		Short: "Stop requiring a TOTP code at login",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, st, err := a.openHub(*dirFlag)
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.SetTOTP(""); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "TOTP disabled; logins need the password only")
			return nil
		},
	}
	user := group("user", "Manage the hub's user: password and TOTP", false, setPassword, group("totp", "Turn TOTP codes at login on or off", false, totpEnable, totpDisable))

	// --- sessions ---
	var sessJSON bool
	sessLs := &cobra.Command{
		Use:   "ls [--json]",
		Short: "List login sessions",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, st, err := a.openHub(*dirFlag)
			if err != nil {
				return err
			}
			defer st.Close()
			list, err := st.Logins()
			if err != nil {
				return err
			}
			type row struct {
				ID       string    `json:"id"`
				Device   string    `json:"device"`
				Created  time.Time `json:"created"`
				LastUsed time.Time `json:"last_used"`
				Expires  time.Time `json:"expires"`
			}
			rows := []row{}
			for _, l := range list {
				rows = append(rows, row{ID: l.ID[:12], Device: l.Device, Created: l.Created, LastUsed: l.LastUsed, Expires: l.Expires()})
			}
			if sessJSON {
				return writeJSON(a.stdout, rows)
			}
			tw := table(a.stdout, "ID", "DEVICE", "CREATED", "LAST USED", "EXPIRES")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Device, fmtTime(r.Created), fmtTime(r.LastUsed), fmtTime(r.Expires))
			}
			return tw.Flush()
		},
	}
	sessLs.Flags().BoolVar(&sessJSON, "json", false, "print JSON")
	var revokeAll bool
	sessRevoke := &cobra.Command{
		Use:   "revoke <id|device> | --all",
		Short: "End login sessions: by ID (at least 8 characters), by device name, or all",
		Args: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 1) == revokeAll || len(args) > 1 {
				return usageErrorf("usage: devc hub sessions revoke <id|device> | --all")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			_, st, err := a.openHub(*dirFlag)
			if err != nil {
				return err
			}
			defer st.Close()
			match := "*"
			if !revokeAll {
				match = args[0]
				if match == "*" || match == "" {
					return usageErrorf("give a session ID, a device name or --all")
				}
			}
			n, err := st.DeleteLogins(match)
			if err != nil {
				return err
			}
			if n == 0 && !revokeAll {
				return fmt.Errorf("no session matches %q ('devc hub sessions ls')", match)
			}
			fmt.Fprintf(a.stdout, "ended %d session(s)\n", n)
			return nil
		},
	}
	sessRevoke.Flags().BoolVar(&revokeAll, "all", false, "end every session")
	sessions := group("sessions", "List and end login sessions", false, sessLs, sessRevoke)

	// --- devices and machines ---
	registry := func(role string) *cobra.Command {
		var asJSON bool
		ls := &cobra.Command{
			Use:   "ls [--json]",
			Short: "List the " + role + " certificates with their last use",
			Args:  noArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				_, st, err := a.openHub(*dirFlag)
				if err != nil {
					return err
				}
				defer st.Close()
				certs, err := st.Certs()
				if err != nil {
					return err
				}
				seen, err := st.CertsSeen()
				if err != nil {
					return err
				}
				logins, err := st.Logins()
				if err != nil {
					return err
				}
				machines := map[string]hub.MachineRow{}
				if role == hub.RoleMachine {
					ms, err := st.Machines()
					if err != nil {
						return err
					}
					for _, m := range ms {
						machines[m.Name] = m
					}
				}
				type row struct {
					Name        string     `json:"name"`
					Fingerprint string     `json:"fingerprint"`
					Issued      time.Time  `json:"issued"`
					Expires     time.Time  `json:"expires"`
					State       string     `json:"state"`
					LastSeen    *time.Time `json:"last_seen,omitempty"`
					Sessions    int        `json:"sessions,omitempty"`
					Online      bool       `json:"online,omitempty"`
					Host        string     `json:"host,omitempty"`
				}
				now := time.Now()
				rows := []row{}
				for _, c := range certs {
					if c.Role != role {
						continue
					}
					r := row{Name: c.Name, Fingerprint: c.Fingerprint, Issued: c.Issued, Expires: c.Expires, State: certState(c, now)}
					if t, ok := seen[c.Fingerprint]; ok {
						r.LastSeen = &t
					}
					for _, l := range logins {
						if l.Fingerprint == c.Fingerprint {
							r.Sessions++
						}
					}
					if m, ok := machines[c.Name]; ok && m.Fingerprint == c.Fingerprint {
						r.Online, r.Host = m.Online, m.Host
						if m.LastSeen != nil && (r.LastSeen == nil || m.LastSeen.After(*r.LastSeen)) {
							r.LastSeen = m.LastSeen
						}
					}
					rows = append(rows, r)
				}
				if asJSON {
					return writeJSON(a.stdout, rows)
				}
				if role == hub.RoleMachine {
					tw := table(a.stdout, "NAME", "FINGERPRINT", "EXPIRES", "STATE", "ONLINE", "LAST SEEN", "HOST")
					for _, r := range rows {
						online := "no"
						if r.Online {
							online = "yes"
						}
						last := "-"
						if r.LastSeen != nil {
							last = fmtTime(*r.LastSeen)
						}
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Fingerprint[:16], r.Expires.Local().Format("2006-01-02"), r.State, online, last, oneLine(r.Host, 40))
					}
					return tw.Flush()
				}
				tw := table(a.stdout, "NAME", "FINGERPRINT", "EXPIRES", "STATE", "LAST SEEN", "SESSIONS")
				for _, r := range rows {
					last := "-"
					if r.LastSeen != nil {
						last = fmtTime(*r.LastSeen)
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", r.Name, r.Fingerprint[:16], r.Expires.Local().Format("2006-01-02"), r.State, last, r.Sessions)
				}
				return tw.Flush()
			},
		}
		ls.Flags().BoolVar(&asJSON, "json", false, "print JSON")
		revoke := &cobra.Command{
			Use:   "revoke <name>",
			Short: "Revoke a " + role + "'s certificates; its connections and login sessions end at once",
			Args: func(cmd *cobra.Command, args []string) error {
				if len(args) != 1 {
					return usageErrorf("usage: devc hub %ss revoke <name>", role)
				}
				return nil
			},
			RunE: func(cmd *cobra.Command, args []string) error {
				_, st, err := a.openHub(*dirFlag)
				if err != nil {
					return err
				}
				defer st.Close()
				logins, err := st.Logins()
				if err != nil {
					return err
				}
				hit, err := st.Revoke(args[0], role)
				if err != nil {
					return err
				}
				if len(hit) == 0 {
					return fmt.Errorf("no valid %s certificate named %q ('devc hub %ss ls')", role, args[0], role)
				}
				for _, c := range hit {
					n := 0
					for _, l := range logins {
						if l.Fingerprint == c.Fingerprint {
							n++
						}
					}
					fmt.Fprintf(a.stdout, "revoked the %s certificate of %s (%s)", role, c.Name, c.Fingerprint[:16])
					if role == hub.RoleDevice {
						fmt.Fprintf(a.stdout, "; %d login session(s) ended", n)
					}
					fmt.Fprintln(a.stdout)
				}
				return nil
			},
		}
		return group(role+"s", "List and revoke "+role+"s", false, ls, revoke)
	}

	// --- enrol ---
	var ttl time.Duration
	var legacy bool
	enroll := &cobra.Command{
		Use:   "enroll machine|device <name> [--ttl 15m]",
		Short: "Make a single-use enrolment token (valid for at most 15 minutes)",
		Long: "Make a single-use token for a new machine or device. While a token is outstanding, the hub serves " +
			"/enroll/<token> without a client certificate. A machine runs 'devc join <hub-url> --token <token>': it " +
			"makes its key itself and receives a certificate. A device opens the printed URL in its browser and " +
			"downloads a PKCS#12 bundle whose passphrase is printed here only.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 2 {
				return usageErrorf("usage: devc hub enroll machine|device <name>")
			}
			if args[0] != hub.RoleMachine && args[0] != hub.RoleDevice {
				return usageErrorf("the role is machine or device, not %q", args[0])
			}
			if !names.Valid(args[1]) {
				return usageErrorf("%q is not a valid name (a-z, 0-9, '.', '_', '-'; up to 63 characters)", args[1])
			}
			if ttl <= 0 || ttl > hub.EnrolTTL {
				return usageErrorf("--ttl must be between 1s and %s", hub.EnrolTTL)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			l, st, err := a.openHub(*dirFlag)
			if err != nil {
				return err
			}
			defer st.Close()
			pub, err := hub.PublicURL(l, st)
			if err != nil {
				return err
			}
			defer syscall.Umask(syscall.Umask(0o077))
			en, err := hub.CreateEnrolment(l, st, args[0], args[1], ttl, legacy)
			if err != nil {
				return err
			}
			until := en.Expires.Local().Format("15:04:05")
			if en.Role == hub.RoleMachine {
				fmt.Fprintf(a.stdout, "enrolment token for machine %s: single use, valid until %s\non the machine:\n  devc join %s --token %s\n", en.Name, until, pub, en.Token)
				fmt.Fprintf(a.stdout, "when the hub serves the server certificate of 'devc hub init', add --ca with a copy of %s\n", l.CACert())
				return nil
			}
			fmt.Fprintf(a.stdout, "enrolment for device %s: single use, valid until %s\nopen on the device:\n  %s/enroll/%s\n", en.Name, until, pub, en.Token)
			fmt.Fprintf(a.stdout, "passphrase of the bundle (shown only here): %s\n", en.Passphrase)
			return nil
		},
	}
	enroll.Flags().DurationVar(&ttl, "ttl", hub.EnrolTTL, "how long the token is valid (at most 15m)")
	enroll.Flags().BoolVar(&legacy, "legacy-p12", false, "encrypt a device's bundle with the older algorithms some phones need")

	// --- doctor ---
	var docJSON bool
	doctor := &cobra.Command{
		Use:   "doctor [--json]",
		Short: "Check the hub directory's permissions, the password, the public URL and ntfy",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := a.hubDir(*dirFlag)
			if err != nil {
				return err
			}
			l := hub.Layout{Dir: dir}
			if _, err := os.Stat(l.CACert()); err != nil {
				return fmt.Errorf("%s is not a hub directory ('devc hub init')", dir)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			fs := hub.Doctor(ctx, l, "")
			failed := false
			for _, f := range fs {
				failed = failed || !f.OK
			}
			if docJSON {
				if err := writeJSON(a.stdout, fs); err != nil {
					return err
				}
			} else {
				for _, f := range fs {
					mark := "ok  "
					switch {
					case !f.OK:
						mark = "FAIL"
					case f.Warn:
						mark = "warn"
					}
					fmt.Fprintf(a.stdout, "%s  %s: %s\n", mark, f.Check, f.Detail)
				}
			}
			if failed {
				return exitCode(1)
			}
			return nil
		},
	}
	doctor.Flags().BoolVar(&docJSON, "json", false, "print JSON")

	return []*cobra.Command{user, sessions, registry(hub.RoleDevice), registry(hub.RoleMachine), enroll, doctor}
}

// joinWithToken enrols this machine at a hub: a key made here, a
// certificate request to /enroll/<token>, the certificate stored.
func (a *app) joinWithToken(ctx context.Context, hubURL, token string, caPEM []byte) (certPEM, keyPEM []byte, err error) {
	if token == "-" {
		if token, err = a.readLine(256); err != nil {
			return nil, nil, err
		}
		token = strings.TrimSpace(token)
	}
	if !hub.ValidToken(token) {
		return nil, nil, usageErrorf("the token is not of the form 'devc hub enroll' prints")
	}
	keyPEM, csrPEM, err := hub.NewMachineCSR()
	if err != nil {
		return nil, nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caPEM != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, nil, errors.New("the CA file holds no PEM certificate")
		}
		cfg.RootCAs = pool
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: cfg}}
	body, _ := json.Marshal(hub.EnrolMachineRequest{CSR: string(csrPEM)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/enroll/"+token, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hub.RequestHeader, "1")
	resp, err := client.Do(req)
	if err != nil {
		// A hub in direct TLS mode refuses connections without a client
		// certificate at the handshake while no token is outstanding.
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Op == "remote error" {
			return nil, nil, errors.New("the hub refused a connection without a certificate: the token is unknown, used or expired (tokens work once, for at most 15 minutes)")
		}
		// The error names the URL, which holds the token: say less.
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			err = ue.Unwrap()
		}
		return nil, nil, fmt.Errorf("reaching the hub: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil, errors.New("the hub does not accept this token: it is unknown, used or expired (tokens work once, for at most 15 minutes)")
	default:
		return nil, nil, fmt.Errorf("the hub answered %s: %s", resp.Status, oneLine(string(b), 200))
	}
	var rep hub.EnrolMachineReply
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, nil, fmt.Errorf("the hub's answer is invalid: %w", err)
	}
	return []byte(rep.Certificate), keyPEM, nil
}
