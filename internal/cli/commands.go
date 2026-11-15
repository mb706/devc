package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/doctor"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/version"
)

func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the devc version, Go version and platform",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(a.stdout, version.String())
			return nil
		},
	}
}

func newDoctorCmd(a *app) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check host requirements",
		Long:  "Check the host requirements devc depends on. Nothing is changed; fixes are printed as hints.",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			name, uid := currentUser(a.getenv)
			eng := engine.NewPodman(c.Global.Engine, a.runner)
			h := doctor.NewHost(a.runner, eng, name, uid, c.Paths.ProjectsRoot(), c.Paths.StateDir())
			h.Getenv = a.getenv
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
			defer cancel()
			checks := doctor.Run(ctx, h)
			if asJSON {
				if err := writeJSON(a.stdout, checks); err != nil {
					return err
				}
			} else if err := doctor.WriteTable(a.stdout, checks); err != nil {
				return err
			}
			if doctor.Failed(checks) {
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the checks as JSON")
	return cmd
}

// currentUser returns the login name and UID. os/user reads /etc/passwd
// without cgo; $USER covers accounts from other sources.
func currentUser(getenv func(string) string) (string, int) {
	uid := os.Getuid()
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username, uid
	}
	if n := getenv("USER"); n != "" {
		return n, uid
	}
	return strconv.Itoa(uid), uid
}

func newConfigCmd(a *app) *cobra.Command {
	check := &cobra.Command{
		Use:   "check",
		Short: "Load the whole configuration and report every problem",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			if fi, err := os.Stat(c.Paths.ConfigDir()); err != nil || !fi.IsDir() {
				return fmt.Errorf("configuration directory %s does not exist: create it or pass --config-dir", c.Paths.ConfigDir())
			}
			for _, p := range probs {
				p.File = display(c, p.File)
				fmt.Fprintln(a.stdout, p.String())
			}
			if len(probs) > 0 {
				fmt.Fprintf(a.stderr, "devc: %d problem(s) in %s\n", len(probs), c.Paths.ConfigDir())
				return exitCode(1)
			}
			fmt.Fprintf(a.stdout, "configuration OK: %d realm(s), %d project(s), %d env(s), %d harness(es) in %s\n",
				len(c.Realms()), len(c.Projects("")), len(c.Envs()), len(c.Harnesses()), c.Paths.ConfigDir())
			return nil
		},
	}

	var asJSON bool
	show := &cobra.Command{
		Use:   "show [<realm>/<project>]",
		Short: "Print the resolved configuration with defaults applied",
		Long: "Without an argument, print the global settings and effective directories. With a project " +
			"(<realm>/<project> or a unique project name), print its configuration with project, realm and " +
			"global settings merged.",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			var v any = c.View()
			if len(args) == 1 {
				ref, err := c.ResolveProject(args[0], "")
				if err != nil {
					return err
				}
				for _, p := range c.ProblemsFor(ref) {
					p.File = display(c, p.File)
					fmt.Fprintf(a.stderr, "devc: warning: %s\n", p)
				}
				r, err := c.Resolved(ref)
				if err != nil {
					return err
				}
				v = r
			} else {
				a.warnProblems(probs)
			}
			if asJSON {
				return writeJSON(a.stdout, v)
			}
			return toml.NewEncoder(a.stdout).Encode(v)
		},
	}
	show.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of TOML")

	return group("config", "Inspect the configuration", false, check, show)
}

type realmRow struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Network      string   `json:"network"`
	SELinuxLevel string   `json:"selinux_level"`
	SyncHub      bool     `json:"sync_hub"`
	Projects     []string `json:"projects"`
}

func newRealmCmd(a *app) *cobra.Command {
	var asJSON bool
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List realms",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			rows := []realmRow{}
			for _, r := range c.Realms() {
				row := realmRow{Name: r.Name, Description: r.Description, Network: r.Network.Default,
					SELinuxLevel: r.SELinux.Level, SyncHub: r.Sync.Hub, Projects: []string{}}
				for _, p := range c.Projects(r.Name) {
					row.Projects = append(row.Projects, p.Name)
				}
				rows = append(rows, row)
			}
			if asJSON {
				return writeJSON(a.stdout, rows)
			}
			tw := table(a.stdout, "REALM", "PROJECTS", "NETWORK", "DESCRIPTION")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", r.Name, len(r.Projects), r.Network, r.Description)
			}
			return tw.Flush()
		},
	}
	ls.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return group("realm", "Manage realms", false, ls)
}

type projectRow struct {
	Realm     string   `json:"realm"`
	Project   string   `json:"project"`
	Env       string   `json:"env"`
	Image     string   `json:"image"`
	Harnesses []string `json:"harnesses"`
	Network   string   `json:"network"`
	Data      bool     `json:"data"`
	BoxName   string   `json:"box_name"`
	HostDir   string   `json:"host_dir"`
}

func newProjectCmd(a *app) *cobra.Command {
	var asJSON bool
	var realm string
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List projects",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			if realm != "" {
				if err := names.Check(names.KindRealm, realm); err != nil {
					return &usageError{msg: err.Error()}
				}
				if _, ok := c.Realm(realm); !ok {
					return fmt.Errorf("no realm %q: run 'devc realm ls' to list realms", realm)
				}
			}
			a.warnProblems(probs)
			rows := []projectRow{}
			for _, p := range c.Projects(realm) {
				r, err := c.Resolved(p.Ref())
				if err != nil {
					return err
				}
				row := projectRow{Realm: r.Realm, Project: r.Project, Env: r.Env, Image: r.Image, Network: r.Network,
					Data: r.Data.Enabled, BoxName: r.BoxName, HostDir: r.HostDir, Harnesses: []string{}}
				for _, h := range r.Harnesses {
					row.Harnesses = append(row.Harnesses, h.ID)
				}
				rows = append(rows, row)
			}
			if asJSON {
				return writeJSON(a.stdout, rows)
			}
			tw := table(a.stdout, "PROJECT", "IMAGE", "HARNESSES", "NETWORK", "DATA")
			for _, r := range rows {
				img := "env:" + r.Env
				if r.Image != "" {
					img = r.Image
				}
				data := "-"
				if r.Data {
					data = "yes"
				}
				fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%s\t%s\n", r.Realm, r.Project, img, strings.Join(r.Harnesses, ","), r.Network, data)
			}
			return tw.Flush()
		},
	}
	ls.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	ls.Flags().StringVar(&realm, "realm", "", "only list projects of this realm")
	return group("project", "Manage projects", false, ls)
}

func table(w io.Writer, headers ...string) *tabwriter.Writer {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	return tw
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
