// This file implements the containers the archive needs: the archive
// worker in the helper container, which lists or packs the box-writable
// harness state so the host never walks it, and the context worker.

package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/ctxrepo"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/toolbox"
)

// Workers run the containers the archive needs: the helper container's
// archive worker over the box-writable harness state, and the context
// worker for the context repository.
type Workers interface {
	ListHarness(ctx context.Context, realm string, req *boxside.ArchiveRequest) (*boxside.ArchiveListing, error)
	TarHarness(ctx context.Context, realm string, req *boxside.ArchiveRequest, w io.Writer) error
	ContextBundle(ctx context.Context, realm string, excludes []string, dir string) (*ctxrepo.BundleResult, error)
	ContextRestore(ctx context.Context, realm string, refs map[string]string, dir string) (*ctxrepo.RestoreResult, error)
}

// HarnessRequest is the archive worker's request for the realm: every
// harness the configuration knows, with its credential patterns. State
// of a harness without a manifest is not read (its credential files are
// unknown).
func HarnessRequest(c *config.Config, realm, op string) *boxside.ArchiveRequest {
	req := &boxside.ArchiveRequest{V: boxside.ArchiveProtoVersion, Op: op, MaxFile: 256 << 20, Harnesses: map[string][]string{}}
	if r, ok := c.Realm(realm); ok {
		req.MaxFile = r.ArchiveMaxFile()
	}
	for _, m := range c.Harnesses() {
		pats := m.SecretStatePatterns()
		if pats == nil {
			pats = []string{}
		}
		req.Harnesses[m.ID] = pats
	}
	return req
}

// ContainerWorkers runs the workers with the configuration's engine.
type ContainerWorkers struct {
	Config     *config.Config
	Engine     engine.Engine
	Executable string
	Version    string
	// Out receives progress messages (an image build).
	Out io.Writer

	once    sync.Once
	syncErr error
}

// workerTimeout bounds one archive worker.
const workerTimeout = 30 * time.Minute

func (w *ContainerWorkers) toolbox() (*toolbox.Toolbox, error) {
	tb := toolbox.New(w.Config.Paths.ToolboxDir())
	w.once.Do(func() {
		if err := w.Config.Paths.Ensure(tb.Dir); err != nil {
			w.syncErr = err
			return
		}
		if _, err := tb.Sync(w.Executable, w.Version); err != nil {
			w.syncErr = fmt.Errorf("syncing the toolbox: %w", err)
		}
	})
	return tb, w.syncErr
}

func (w *ContainerWorkers) helper(realm string, req *boxside.ArchiveRequest) (engine.RunOptions, bool, error) {
	dir := filepath.Join(w.Config.Paths.DurableRealmDir(realm), "harness")
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return engine.RunOptions{}, false, nil
	}
	tb, err := w.toolbox()
	if err != nil {
		return engine.RunOptions{}, false, err
	}
	in, err := json.Marshal(req)
	if err != nil {
		return engine.RunOptions{}, false, err
	}
	selinux, disable := false, false
	if _, err := os.Stat("/sys/fs/selinux/enforce"); err == nil {
		selinux = true
		disable = w.Config.Global.SELinux.Mode == config.SELinuxDisable
	}
	opts := tb.AuxSecurityOpts(disable)
	// The realm's state carries the realm's level on SELinux hosts, as the
	// data workers' do.
	if r, ok := w.Config.Realm(realm); ok && selinux && !disable && r.SELinux.Level != "" {
		opts = append(opts, "label=level:"+r.SELinux.Level)
	}
	return engine.RunOptions{
		Rootfs:  tb.HelperRootfs(),
		Network: "none",
		UserNS:  "keep-id:uid=1000,gid=1000",
		Labels:  map[string]string{"devc.realm": realm, "devc.role": "helper"},
		// Reading every file of the state, whoever in the box made it.
		User: "0:0", CapDropAll: true, CapAdd: []string{"DAC_READ_SEARCH"},
		SecurityOpts: opts,
		Mounts:       []engine.RunMount{{Source: dir, Target: boxside.HelperTarget, ReadOnly: true}},
		Env:          []string{"DEVC_HELPER=1"},
		Cmd:          []string{"/bin/devc", "box", "archive-worker"},
		Interactive:  true,
		Stdin:        bytes.NewReader(in),
		Timeout:      workerTimeout,
	}, true, nil
}

func workerFailed(err error, stderr []byte) error {
	var ee *execx.ExitError
	if !errors.As(err, &ee) {
		return fmt.Errorf("running the archive worker: %w", err)
	}
	s := strings.TrimSpace(string(stderr))
	if len(s) > 1000 {
		s = s[len(s)-1000:]
	}
	return fmt.Errorf("the archive worker failed: %s", proto.PrintableText(s))
}

// ListHarness implements Workers.
func (w *ContainerWorkers) ListHarness(ctx context.Context, realm string, req *boxside.ArchiveRequest) (*boxside.ArchiveListing, error) {
	o, ok, err := w.helper(realm, req)
	if err != nil || !ok {
		return &boxside.ArchiveListing{V: boxside.ArchiveProtoVersion}, err
	}
	// The capture matches what the worker may print and the decoder
	// accepts (finding D1-16); a cut report is an error, never a listing.
	o.MaxOutput = boxside.MaxArchiveListing
	res, err := w.Engine.Run(ctx, o)
	if err != nil {
		return nil, workerFailed(err, res.Stderr)
	}
	if res.Truncated {
		return nil, fmt.Errorf("the archive worker's listing is larger than %d bytes", boxside.MaxArchiveListing)
	}
	return boxside.DecodeArchiveListing(res.Stdout)
}

// TarHarness implements Workers: the worker's tar stream goes to out.
func (w *ContainerWorkers) TarHarness(ctx context.Context, realm string, req *boxside.ArchiveRequest, out io.Writer) error {
	o, ok, err := w.helper(realm, req)
	if err != nil {
		return err
	}
	if !ok {
		return errNoHarnessState
	}
	var stderr bytes.Buffer
	o.Stdout, o.Stderr = out, &limitedBuffer{b: &stderr, n: 64 << 10}
	code, err := w.Engine.RunStream(ctx, o)
	if err != nil {
		return fmt.Errorf("running the archive worker: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("the archive worker failed (exit %d): %s", code, proto.PrintableText(strings.TrimSpace(stderr.String())))
	}
	return nil
}

// errNoHarnessState says the realm has no harness state directory.
var errNoHarnessState = errors.New("no harness state")

type limitedBuffer struct {
	b *bytes.Buffer
	n int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}

func (w *ContainerWorkers) repo() *ctxrepo.Repo {
	return ctxrepo.New(w.Config, w.Engine, w.Executable, w.Version, w.Out)
}

// ContextBundle implements Workers.
func (w *ContainerWorkers) ContextBundle(ctx context.Context, realm string, excludes []string, dir string) (*ctxrepo.BundleResult, error) {
	return w.repo().Bundle(ctx, realm, excludes, dir)
}

// ContextRestore implements Workers.
func (w *ContainerWorkers) ContextRestore(ctx context.Context, realm string, refs map[string]string, dir string) (*ctxrepo.RestoreResult, error) {
	return w.repo().Restore(ctx, realm, refs, dir)
}
