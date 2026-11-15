package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/toolbox"
	"github.com/mb706/devc/internal/version"
)

func newToolboxCmd(a *app) *cobra.Command {
	sync := &cobra.Command{
		Use:   "sync",
		Short: "Install this devc into the toolbox and write the seccomp profile",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			exe, err := a.executable()
			if err != nil {
				return err
			}
			if err := c.Paths.Ensure(c.Paths.ToolboxDir()); err != nil {
				return err
			}
			res, err := toolbox.New(c.Paths.ToolboxDir()).Sync(exe, version.Version)
			if err != nil {
				return err
			}
			state := "up to date"
			if res.Changed {
				state = "updated"
			}
			fmt.Fprintf(a.stdout, "toolbox %s: %s (devc %s)\n", c.Paths.ToolboxDir(), state, res.ID)
			return nil
		},
	}

	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls",
		Short: "Show what the toolbox contains",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			info, err := toolbox.New(c.Paths.ToolboxDir()).List()
			if err != nil {
				return err
			}
			if lsJSON {
				return writeJSON(a.stdout, info)
			}
			fmt.Fprintf(a.stdout, "toolbox  %s\n", info.Dir)
			for _, b := range info.Builds {
				mark := " "
				if b == info.Current {
					mark = "*"
				}
				fmt.Fprintf(a.stdout, "devc   %s %s\n", mark, b)
			}
			fmt.Fprintf(a.stdout, "helper   %s\n", okText(info.Helper))
			fmt.Fprintf(a.stdout, "seccomp  %s\n", okText(info.Seccomp))
			switch {
			case info.Tmux == "":
				fmt.Fprintf(a.stdout, "tmux     missing: run 'devc toolbox build tmux'\n")
			case info.Tmux != info.TmuxPinned:
				fmt.Fprintf(a.stdout, "tmux     %s (this devc builds %s: run 'devc toolbox build tmux')\n", info.Tmux, info.TmuxPinned)
			default:
				fmt.Fprintf(a.stdout, "tmux     %s\n", info.Tmux)
			}
			if len(info.Shims) > 0 {
				fmt.Fprintf(a.stdout, "shims    %s\n", strings.Join(info.Shims, " "))
			}
			for _, p := range info.Problems {
				fmt.Fprintf(a.stderr, "devc: toolbox: %s\n", p)
			}
			return nil
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")
	return group("toolbox", "Manage the toolbox mounted into every box", false, sync, ls, newToolboxBuildCmd(a))
}

func okText(ok bool) string {
	if ok {
		return "ok"
	}
	return "missing or outdated"
}

type envRow struct {
	Name             string     `json:"name"`
	HasContainerfile bool       `json:"has_containerfile"`
	Base             string     `json:"base"`
	Description      string     `json:"description"`
	Dir              string     `json:"dir"`
	Image            string     `json:"image"`
	ImageExists      bool       `json:"image_exists"`
	ImageID          string     `json:"image_id,omitempty"`
	Built            *time.Time `json:"built,omitempty"`
}

func newEnvCmd(a *app) *cobra.Command {
	var asJSON bool
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List envs (image recipes) and whether their images exist",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			eng := a.engine(c)
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			rows := []envRow{}
			var engineErr error
			for _, e := range c.Envs() {
				row := envRow{Name: e.Name, HasContainerfile: e.HasContainerfile, Base: e.Base, Description: e.Description, Dir: e.Dir}
				row.Image = e.Base
				if e.HasContainerfile {
					row.Image = box.EnvImageTag(e.Name)
				}
				if row.Image != "" && engineErr == nil {
					ii, err := eng.ImageInspect(ctx, row.Image)
					switch {
					case err == nil:
						row.ImageExists, row.ImageID = true, ii.ID
						t := ii.Created
						row.Built = &t
					case !errors.Is(err, engine.ErrNoSuchImage):
						engineErr = err
					}
				}
				rows = append(rows, row)
			}
			if engineErr != nil {
				fmt.Fprintf(a.stderr, "devc: warning: cannot query images: %v\n", engineErr)
			}
			if asJSON {
				return writeJSON(a.stdout, rows)
			}
			tw := table(a.stdout, "ENV", "SOURCE", "IMAGE", "BUILT", "DESCRIPTION")
			for _, e := range rows {
				src := "Containerfile"
				if !e.HasContainerfile {
					src = "image " + e.Base
				}
				img, built := "missing", "-"
				if e.ImageExists {
					img = "present"
					built = e.Built.Local().Format("2006-01-02 15:04")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Name, src, img, built, e.Description)
			}
			return tw.Flush()
		},
	}
	ls.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	var noCache, pull bool
	build := &cobra.Command{
		Use:   "build <env>",
		Short: "Build an env's image from its Containerfile",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageErrorf("%q takes exactly one env name", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			if err := names.Check(names.KindEnv, args[0]); err != nil {
				return &usageError{msg: err.Error()}
			}
			e, ok := c.Env(args[0])
			if !ok {
				return fmt.Errorf("no env %q: run 'devc env ls' to list envs", args[0])
			}
			var mine []string
			for _, p := range probs {
				if strings.HasPrefix(p.File, e.Dir+"/") || p.File == e.Dir {
					mine = append(mine, display(c, p.File)+": "+p.Message)
				}
			}
			if len(mine) > 0 {
				for _, m := range mine {
					fmt.Fprintf(a.stderr, "devc: %s\n", m)
				}
				return fmt.Errorf("env %s has configuration problems; fix them first", e.Name)
			}
			eng := a.engine(c)
			fmt.Fprintf(a.stderr, "devc: building env %s as %s\n", e.Name, box.EnvImageTag(e.Name))
			if err := box.BuildEnv(cmd.Context(), eng, c, e, box.BuildEnvOptions{NoCache: noCache, Pull: pull}, a.stderr); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "built %s\n", box.EnvImageTag(e.Name))
			return nil
		},
	}
	build.Flags().BoolVar(&noCache, "no-cache", false, "do not use cached layers")
	build.Flags().BoolVar(&pull, "pull", false, "always pull the base images")
	return group("env", "Manage envs", false, ls, build)
}
