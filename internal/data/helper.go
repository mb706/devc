package data

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/proto"
)

// Labels on helper containers (plan 3.1).
const (
	labelRealm   = "devc.realm"
	labelProject = "devc.project"
	labelRole    = "devc.role"
	roleHelper   = "helper"
)

// boxUserNS is the user-namespace mapping of every box; helper containers
// use it too, so in-box UIDs 1000 and 2000 are the same host IDs.
const boxUserNS = "keep-id:uid=1000,gid=1000"

// Capability sets of the workers (work order C06 section 2).
var (
	capsOwner = []string{"CHOWN", "FOWNER", "DAC_OVERRIDE"}
	capsRead  = []string{"DAC_READ_SEARCH"}
)

// maxResult caps a worker's JSON result.
const maxResult = 8 << 20

// workerTimeout bounds one worker run; copies of large trees take time.
const workerTimeout = 6 * time.Hour

func (s *Service) syncToolbox() error {
	s.syncOnce.Do(func() {
		if err := s.Config.Paths.Ensure(s.Toolbox.Dir); err != nil {
			s.syncErr = err
			return
		}
		if _, err := s.Toolbox.Sync(s.Executable, s.Version); err != nil {
			s.syncErr = fmt.Errorf("syncing the toolbox: %w", err)
		}
	})
	return s.syncErr
}

// securityOpts are the box's: no new privileges, the box seccomp profile,
// and on SELinux hosts the realm's level (or label=disable).
func (s *Service) securityOpts(r *config.Resolved) []string {
	root := s.HostRoot
	if root == "" {
		root = "/"
	}
	selinux := false
	if _, err := os.Stat(filepath.Join(root, "sys", "fs", "selinux", "enforce")); err == nil {
		selinux = true
	}
	disable := selinux && s.Config.Global.SELinux.Mode == config.SELinuxDisable
	opts := s.Toolbox.AuxSecurityOpts(disable)
	if selinux && !disable && r.SELinuxLevel != "" {
		opts = append(opts, "label=level:"+r.SELinuxLevel)
	}
	return opts
}

func (s *Service) helperBase(p *Project) engine.RunOptions {
	return engine.RunOptions{
		Rootfs:       s.Toolbox.HelperRootfs(),
		Network:      "none",
		UserNS:       boxUserNS,
		Labels:       map[string]string{labelRealm: p.Ref.Realm, labelProject: p.Ref.Project, labelRole: roleHelper},
		SecurityOpts: s.securityOpts(p.Resolved),
		CapDropAll:   true,
		Timeout:      workerTimeout,
	}
}

// work runs one data request in the helper container with the project
// directory at /target (and staging, when set, read-only at /staging).
func (s *Service) work(ctx context.Context, p *Project, req boxside.DataRequest, caps []string, staging string) (*boxside.DataResult, error) {
	if err := s.syncToolbox(); err != nil {
		return nil, err
	}
	// The project directory is host-made (its realm directory private);
	// everything below it is the worker's business.
	if err := box.EnsureProjectDir(s.Config.Paths, p.HostDir); err != nil {
		return nil, fmt.Errorf("creating the project directory: %w", err)
	}
	req.V = boxside.DataProtoVersion
	req.Layout = p.Layout
	if req.Time == "" {
		req.Time = s.now().Format(time.RFC3339Nano)
	}
	in, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	o := s.helperBase(p)
	o.User = "0:0"
	o.CapAdd = caps
	o.Mounts = []engine.RunMount{{Source: p.HostDir, Target: boxside.HelperTarget}}
	if staging != "" {
		o.Mounts = append(o.Mounts, engine.RunMount{Source: staging, Target: boxside.HelperStaging, ReadOnly: true})
	}
	o.Env = []string{"DEVC_HELPER=1"}
	o.Cmd = []string{"/bin/devc", "box", "data-worker"}
	// The request goes in on stdin (-i).
	o.Interactive = true
	o.Stdin = bytes.NewReader(in)
	res, err := s.Engine.Run(ctx, o)
	if err != nil {
		return nil, workerError(req.Op, err, res)
	}
	out, err := DecodeResult(res.Stdout, req.Op)
	if err != nil {
		return nil, fmt.Errorf("the %s worker's report is invalid: %w", req.Op, err)
	}
	return out, nil
}

// workerError turns a failed worker into an error that carries the
// worker's own message (its stderr lines starting with "devc: "), made
// printable.
func workerError(op string, err error, res execx.Result) error {
	var ee *execx.ExitError
	if !errors.As(err, &ee) {
		return fmt.Errorf("running the %s worker: %w", op, err)
	}
	var msgs []string
	for _, l := range strings.Split(string(res.Stderr), "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(l), "devc: "); ok && m != "" {
			msgs = append(msgs, m)
		}
	}
	if len(msgs) == 0 {
		return fmt.Errorf("the %s worker failed: %s%s", op, proto.PrintableText(err.Error()), tail(res.Stderr))
	}
	msg := strings.Join(msgs, "; ")
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	return errors.New(proto.PrintableText(msg))
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	if len(s) > 1000 {
		s = s[len(s)-1000:]
	}
	return "\n" + proto.PrintableText(s)
}

// DecodeResult decodes and validates a worker's result: no unknown fields,
// nothing after it, the expected operation, valid IDs and paths, sane
// counts. Text fields are made printable.
func DecodeResult(b []byte, op string) (*boxside.DataResult, error) {
	if len(b) > maxResult {
		return nil, fmt.Errorf("larger than %d bytes", maxResult)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r boxside.DataResult
	if err := dec.Decode(&r); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("unexpected data after the report")
	}
	if r.V != boxside.DataProtoVersion || r.Op != op {
		return nil, fmt.Errorf("version %d, operation %q (want %d, %q)", r.V, r.Op, boxside.DataProtoVersion, op)
	}
	if r.Files < 0 || r.Size < 0 {
		return nil, errors.New("negative counts")
	}
	if r.TrashID != "" && !boxside.ValidTrashID(r.TrashID) {
		return nil, fmt.Errorf("invalid trash ID %q", proto.Printable(r.TrashID))
	}
	for _, id := range r.Removed {
		if !boxside.ValidTrashID(id) && !boxside.ValidSavepointID(id) {
			return nil, fmt.Errorf("invalid ID %q", proto.Printable(id))
		}
	}
	checkPaths := func(what string, ps []string) error {
		if len(ps) > 100000 {
			return fmt.Errorf("too many %s", what)
		}
		for _, p := range ps {
			if err := boxside.CheckDataPath(p); err != nil {
				return fmt.Errorf("%s %q: %w", what, proto.Printable(p), err)
			}
		}
		return nil
	}
	for what, ps := range map[string][]string{"paths": r.Paths, "displaced paths": r.Displaced, "excluded paths": r.Exclude, "created paths": r.Created} {
		if err := checkPaths(what, ps); err != nil {
			return nil, err
		}
	}
	if r.Dest != "" {
		if err := boxside.CheckDataPath(r.Dest); err != nil {
			return nil, fmt.Errorf("destination: %w", err)
		}
	}
	for i := range r.Skipped {
		r.Skipped[i] = proto.Printable(r.Skipped[i])
	}
	for _, z := range r.Zones {
		if err := boxside.CheckDataPath(z.Path); err != nil || z.Files < 0 || z.Size < 0 {
			return nil, fmt.Errorf("invalid zone %q", proto.Printable(z.Path))
		}
	}
	for _, e := range r.Entries {
		if err := boxside.CheckDataPath(e.Path); err != nil {
			return nil, fmt.Errorf("invalid entry %q", proto.Printable(e.Path))
		}
	}
	for _, t := range r.Trash {
		if !boxside.ValidTrashID(t.ID) || t.Files < 0 || t.Size < 0 {
			return nil, fmt.Errorf("invalid trash entry %q", proto.Printable(t.ID))
		}
		if err := checkPaths("trash paths", t.Paths); err != nil {
			return nil, err
		}
	}
	for _, sr := range r.Seals {
		if err := boxside.CheckDataPath(sr.Path); err != nil || sr.Files < 0 || sr.Size < 0 {
			return nil, fmt.Errorf("invalid seal record %q", proto.Printable(sr.Path))
		}
	}
	return &r, nil
}
