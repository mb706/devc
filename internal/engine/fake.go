package engine

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Fake is an in-memory Engine for tests of code built on top of the engine.
// Set the exported fields; errors, when set, are returned by the matching
// method.
type Fake struct {
	mu sync.Mutex

	Bin          string
	VersionVal   Version
	VersionErr   error
	InfoVal      Info
	InfoErr      error
	Containers   map[string]ContainerInfo // by name
	ContainerErr error                    // returned by Ps, Inspect and Exists
}

var _ Engine = (*Fake)(nil)

// NewFake returns a rootless Podman 5.8.0 with no containers.
func NewFake() *Fake {
	return &Fake{
		Bin:        "podman",
		VersionVal: Version{Major: 5, Minor: 8, Raw: "5.8.0"},
		InfoVal: Info{
			Rootless: true, NetworkBackend: "netavark", RootlessNetworkCmd: "pasta",
			StorageDriver: "overlay", CgroupVersion: "v2", CgroupControllers: []string{"cpu", "memory", "pids"},
		},
		Containers: map[string]ContainerInfo{},
	}
}

func (f *Fake) Binary() string { return f.Bin }

func (f *Fake) Version(context.Context) (Version, error) { return f.VersionVal, f.VersionErr }

func (f *Fake) Info(context.Context) (Info, error) { return f.InfoVal, f.InfoErr }

// Ps implements Engine.
func (f *Fake) Ps(_ context.Context, o PsOptions) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ContainerErr != nil {
		return nil, f.ContainerErr
	}
	var out []Container
	for name, ci := range f.Containers {
		if !o.All && !ci.State.Running {
			continue
		}
		match := true
		for k, v := range o.Labels {
			got, ok := ci.Labels[k]
			if !ok || (v != "" && got != v) {
				match = false
			}
		}
		if match {
			out = append(out, Container{ID: ci.ID, Names: []string{name}, Image: ci.ImageName, ImageID: ci.Image, State: ci.State.Status, Labels: ci.Labels})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// Inspect implements Engine.
func (f *Fake) Inspect(_ context.Context, name string) (ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ContainerErr != nil {
		return ContainerInfo{}, f.ContainerErr
	}
	ci, ok := f.Containers[name]
	if !ok {
		return ContainerInfo{}, fmt.Errorf("%s: %w", name, ErrNoSuchContainer)
	}
	return ci, nil
}

// Exists implements Engine.
func (f *Fake) Exists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ContainerErr != nil {
		return false, f.ContainerErr
	}
	_, ok := f.Containers[name]
	return ok, nil
}
