package box

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
)

// EnvImageTag is the tag of an env built from a Containerfile.
func EnvImageTag(env string) string {
	if !names.Valid(env) {
		panic(fmt.Sprintf("box: unvalidated env name %q", env))
	}
	return "localhost/devc-env/" + env + ":latest"
}

// ImageSource says where a project's image comes from.
type ImageSource struct {
	Ref string
	// Env is set when the project uses an env; Build is true when that env
	// is built from its Containerfile.
	Env   *config.Env
	Build bool
}

// SourceFor returns the image source of a project.
func SourceFor(c *config.Config, r *config.Resolved) (ImageSource, error) {
	if r.BuildFromRepo != "" {
		return ImageSource{}, errors.New("build_from_repo is not supported yet: remove it from the project definition and use an env")
	}
	if r.Image != "" {
		return ImageSource{Ref: r.Image}, nil
	}
	e, ok := c.Env(r.Env)
	if !ok {
		return ImageSource{}, fmt.Errorf("env %q does not exist", r.Env)
	}
	if e.HasContainerfile {
		return ImageSource{Ref: EnvImageTag(e.Name), Env: e, Build: true}, nil
	}
	if e.Base == "" {
		return ImageSource{}, fmt.Errorf("env %q has neither a Containerfile nor a base image", e.Name)
	}
	return ImageSource{Ref: e.Base, Env: e}, nil
}

// BuildEnvOptions are the switches of `devc env build`.
type BuildEnvOptions struct {
	NoCache bool
	Pull    bool
}

// BuildEnv builds an env's Containerfile with the env directory as context.
func BuildEnv(ctx context.Context, eng engine.Engine, c *config.Config, e *config.Env, o BuildEnvOptions, out io.Writer) error {
	if !e.HasContainerfile {
		return fmt.Errorf("env %q has no Containerfile; it uses the image %s as is", e.Name, e.Base)
	}
	return eng.Build(ctx, engine.BuildOptions{
		ContextDir: c.Paths.EnvDir(e.Name),
		File:       c.Paths.EnvContainerfile(e.Name),
		Tag:        EnvImageTag(e.Name),
		Labels:     map[string]string{LabelRole: "env", "devc.env": e.Name},
		BuildArgs:  e.Build.Args,
		NoCache:    o.NoCache,
		Pull:       o.Pull,
	}, out)
}

// EnsureImage makes the image of src present: an env with a Containerfile
// is built when missing or when build is set; other images are pulled when
// missing. It returns what the engine reports for the image.
func EnsureImage(ctx context.Context, eng engine.Engine, c *config.Config, src ImageSource, build bool, out io.Writer) (engine.ImageInfo, error) {
	exists, err := eng.ImageExists(ctx, src.Ref)
	if err != nil {
		return engine.ImageInfo{}, err
	}
	switch {
	case src.Build && (build || !exists):
		if out != nil {
			fmt.Fprintf(out, "devc: building env %s as %s\n", src.Env.Name, src.Ref)
		}
		if err := BuildEnv(ctx, eng, c, src.Env, BuildEnvOptions{}, out); err != nil {
			return engine.ImageInfo{}, fmt.Errorf("building env %s: %w", src.Env.Name, err)
		}
	case !exists:
		if out != nil {
			fmt.Fprintf(out, "devc: pulling %s\n", src.Ref)
		}
		if err := eng.Pull(ctx, src.Ref, out); err != nil {
			return engine.ImageInfo{}, fmt.Errorf("pulling %s: %w", src.Ref, err)
		}
	}
	return eng.ImageInspect(ctx, src.Ref)
}
