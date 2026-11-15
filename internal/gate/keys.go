package gate

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/flock"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// maxKeyFile bounds the key files the gate reads.
const maxKeyFile = 16 << 10

// Keys makes and removes the gate's keys with ssh-keygen.
type Keys struct {
	Paths  *paths.Paths
	Runner execx.Runner
	// Keygen is the ssh-keygen executable ("ssh-keygen" when empty).
	Keygen string
}

func (k *Keys) keygen() string {
	if k.Keygen == "" {
		return "ssh-keygen"
	}
	return k.Keygen
}

// generate runs ssh-keygen for a new ed25519 key without passphrase in a
// fresh private directory below parent and returns the private key and the
// public key line. The directory is removed afterwards.
func (k *Keys) generate(ctx context.Context, parent, comment string) (priv []byte, pub string, err error) {
	tmp, err := os.MkdirTemp(parent, ".keygen-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(tmp)
	file := filepath.Join(tmp, "key")
	if _, err := k.Runner.Run(ctx, execx.Cmd{Name: k.keygen(), Args: []string{"-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", file}, Timeout: time.Minute}); err != nil {
		if errors.Is(err, execx.ErrNotFound) {
			return nil, "", errors.New("ssh-keygen is not installed; the SSH gate needs it to make its keys (install openssh-client)")
		}
		return nil, "", fmt.Errorf("generating a key with ssh-keygen: %w", err)
	}
	if priv, err = readSmall(file); err != nil {
		return nil, "", err
	}
	b, err := readSmall(file + ".pub")
	if err != nil {
		return nil, "", err
	}
	if pub, err = parsePublicKey(string(b)); err != nil {
		return nil, "", err
	}
	return priv, pub, nil
}

// parsePublicKey returns "ssh-ed25519 <base64>" from a public key line.
func parsePublicKey(line string) (string, error) {
	f := strings.Fields(line)
	if len(f) < 2 || f[0] != "ssh-ed25519" {
		return "", errors.New("ssh-keygen wrote an unexpected public key")
	}
	if _, err := base64.StdEncoding.DecodeString(f[1]); err != nil {
		return "", errors.New("ssh-keygen wrote an unexpected public key")
	}
	return f[0] + " " + f[1], nil
}

func readSmall(p string) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxKeyFile {
		return nil, fmt.Errorf("%s is not a regular file of at most %d bytes", p, maxKeyFile)
	}
	b := make([]byte, fi.Size())
	n, err := f.Read(b)
	if err != nil && n != len(b) {
		return nil, err
	}
	return b[:n], nil
}

// EnsureHostKey creates the gate's host key once per machine and returns
// its public key ("ssh-ed25519 <base64>").
func (k *Keys) EnsureHostKey(ctx context.Context) (string, error) {
	p := k.Paths
	dir := p.GateDir()
	if err := p.Ensure(dir); err != nil {
		return "", err
	}
	unlock, err := flock.Acquire(ctx, filepath.Join(dir, "lock"), flock.Options{Timeout: time.Minute})
	if err != nil {
		return "", err
	}
	defer unlock()
	if pub, err := HostPublicKey(p); err == nil {
		return pub, nil
	}
	priv, pub, err := k.generate(ctx, dir, HostKeyAlias)
	if err != nil {
		return "", err
	}
	// The private key first: a public key on disk means the pair is
	// complete.
	if err := writeAtomic(p.GateHostKey(), priv, 0o600); err != nil {
		return "", err
	}
	if err := writeAtomic(p.GateHostKey()+".pub", []byte(pub+" "+HostKeyAlias+"\n"), 0o644); err != nil {
		return "", err
	}
	return pub, nil
}

// HostPublicKey reads the public half of the gate's host key; it fails
// when the pair does not exist.
func HostPublicKey(p *paths.Paths) (string, error) {
	if _, err := readSmall(p.GateHostKey()); err != nil {
		return "", fmt.Errorf("the gate's host key: %w", err)
	}
	b, err := readSmall(p.GateHostKey() + ".pub")
	if err != nil {
		return "", fmt.Errorf("the gate's host key: %w", err)
	}
	return parsePublicKey(string(b))
}

// NewBoxKey makes a new key pair for a box and returns the private key,
// which the caller delivers into the box's secrets directory. The public
// key replaces the box's authorized-keys file, with restrict,pty. Every
// start of a box gets a new key, so a copy kept from an earlier run is
// worthless. The pair is generated on the runtime directory (a tmpfs), not
// in the state directory, so the private key never reaches persistent
// storage.
func (k *Keys) NewBoxKey(ctx context.Context, boxID string) ([]byte, error) {
	if !names.ValidBoxID(boxID) {
		return nil, fmt.Errorf("invalid box ID %q", boxID)
	}
	p := k.Paths
	rt, err := p.GateRuntimeDir()
	if err != nil {
		return nil, err
	}
	if err := p.Ensure(rt); err != nil {
		return nil, err
	}
	if err := p.Ensure(p.GateKeysDir()); err != nil {
		return nil, err
	}
	comment := "devc-box-" + boxID
	priv, pub, err := k.generate(ctx, rt, comment)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(p.GateBoxKey(boxID), []byte(AuthorizedKeyLine(pub, comment)), 0o600); err != nil {
		return nil, err
	}
	return priv, nil
}

// AuthorizedKeyLine is a box's authorized-keys entry: restrict switches
// off every forwarding, the user rc and the terminal; pty gives the
// terminal back.
func AuthorizedKeyLine(pub, comment string) string {
	return "restrict,pty " + pub + " " + comment + "\n"
}

// RemoveBoxKey removes a box's authorized-keys file.
func RemoveBoxKey(p *paths.Paths, boxID string) error {
	if !names.ValidBoxID(boxID) {
		return fmt.Errorf("invalid box ID %q", boxID)
	}
	if err := os.Remove(p.GateBoxKey(boxID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// writeAtomic writes data to a uniquely named file beside path and renames
// it into place.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
