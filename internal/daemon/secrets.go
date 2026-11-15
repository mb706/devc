package daemon

// On-demand secrets (work order C10a section 3). With secrets.delivery =
// "daemon" nothing is written into a box; its launcher asks host.secret
// for the harness credential and each granted secret. The daemon answers
// only for names the project of the channel is granted: its
// grants.secrets and the secrets of its token harnesses. Values travel in
// the response body only and are never logged.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
)

// GrantedSecrets lists the secrets a project may receive: its granted
// secrets and the credentials of its env-token and api-key harnesses.
func GrantedSecrets(r *config.Resolved) []string {
	out := append([]string{}, r.Grants.Secrets...)
	for _, h := range r.Harnesses {
		if (h.AuthMethod == "env-token" || h.AuthMethod == "api-key") && h.Secret != "" && !slices.Contains(out, h.Secret) {
			out = append(out, h.Secret)
		}
	}
	return out
}

// secret serves host.secret for the box of ref.
func (d *Daemon) secret(_ context.Context, ref names.ProjectRef, params json.RawMessage) (any, error) {
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
