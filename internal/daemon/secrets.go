package daemon

// On-demand secrets (work order C10a section 3). With secrets.delivery =
// "daemon" nothing is written into a box; its launcher asks host.secret
// for the harness credential and each granted secret. The daemon answers
// only for names the project of the channel is granted (its
// grants.secrets and the secrets of its token harnesses) now and was
// granted when its box was started (box.Capabilities, finding C1-02).
// Values travel in the response body only and are never logged.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
)

// GrantedSecrets lists the secrets a project may receive (box.GrantedSecrets).
func GrantedSecrets(r *config.Resolved) []string { return box.GrantedSecrets(r) }

// startedWith returns the capability record of the running box (container
// containerID) of ref, or why there is none usable (finding C1-02).
func (d *Daemon) startedWith(p *paths.Paths, ref names.ProjectRef, containerID string) (*box.Capabilities, *proto.Error) {
	c, err := box.ReadCapabilities(p, ref)
	switch {
	case errors.Is(err, box.ErrNoCapabilities):
		return nil, proto.Errorf(proto.CodeRefused, "this box has no record of what it was started with (it was started by an older devc, or its start did not finish); ask the user to restart it ('devc down %s' and 'devc up %s')", ref, ref)
	case err != nil:
		d.log.Error("reading a box's capability record", "ref", ref.String(), "err", err)
		return nil, proto.Errorf(proto.CodeInternal, "the host could not read what this box was started with")
	case c.ContainerID != containerID:
		return nil, proto.Errorf(proto.CodeRefused, "this box's record of what it was started with belongs to another container; ask the user to restart it ('devc down %s' and 'devc up %s')", ref, ref)
	}
	return c, nil
}

// secret serves host.secret for the box of ref (container containerID).
func (d *Daemon) secret(_ context.Context, ref names.ProjectRef, containerID string, params json.RawMessage) (any, error) {
	var p proto.SecretParams
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if !names.ValidSecret(p.Name) {
		return nil, proto.Errorf(proto.CodeInvalidParams, "invalid secret name")
	}
	e := d.current()
	if e.cfg.Global.Secrets.Delivery != config.DeliveryDaemon {
		return nil, proto.Errorf(proto.CodeRefused, "secrets are delivered as files (secrets.delivery in config.toml); look in /run/devc-secrets")
	}
	r, perr := resolvedOf(e, ref)
	if perr != nil {
		return nil, perr
	}
	if !slices.Contains(GrantedSecrets(r), p.Name) {
		d.log.Warn("refused a secret that is not granted", "ref", ref.String(), "secret", p.Name)
		return nil, proto.Errorf(proto.CodeRefused, "the secret %s is not granted to %s", p.Name, ref)
	}
	caps, perr := d.startedWith(e.cfg.Paths, ref, containerID)
	if perr != nil {
		return nil, perr
	}
	if !caps.AllowsSecret(r, p.Name) {
		d.log.Warn("refused a secret granted after the box started", "ref", ref.String(), "secret", p.Name)
		return nil, proto.Errorf(proto.CodeRefused, "the secret %s was granted to %s after its box started; ask the user to bring the box up again ('devc down %s' and 'devc up %s')", p.Name, ref, ref, ref)
	}
	store := d.o.Secrets
	if store == nil {
		store = secrets.NewFileStore(e.cfg.Paths)
	}
	v, err := store.Get(r.Realm, p.Name)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return nil, proto.Errorf(proto.CodeNotFound, "the secret %s is not set in realm %s: run 'devc secret set %s %s' (or 'devc login') on the host", p.Name, r.Realm, r.Realm, p.Name)
	case err != nil:
		d.log.Error("reading a secret", "ref", ref.String(), "secret", p.Name, "err", err)
		return nil, proto.Errorf(proto.CodeInternal, "the host could not read the secret")
	}
	d.log.Info("secret delivered", "ref", ref.String(), "secret", p.Name)
	return proto.SecretReply{Value: string(v)}, nil
}
