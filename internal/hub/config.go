package hub

// hub.toml in the hub directory (work order C11b section 3): the public URL
// and the ntfy server notifications go to. The file is decoded strictly;
// a key the hub does not know is an error, not ignored.

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is hub.toml.
type Config struct {
	// PublicURL is https://<public name>; it defaults to that.
	PublicURL string         `toml:"public_url"`
	Ntfy      *NtfyConfig    `toml:"ntfy"`
	Terminal  TerminalConfig `toml:"terminal"`
	Archive   ArchiveConfig  `toml:"archive"`
}

// ArchiveConfig bounds what machines may store in the archive (work order
// C17): sizes as "50g", "8m" (units b, k, m, g). Empty values take the
// defaults of DefaultArchiveLimits.
type ArchiveConfig struct {
	// MachineQuota bounds a machine's whole archive, RealmQuota its archive
	// of one realm, MaxFiles the files (and bundles) it may hold.
	MachineQuota string `toml:"machine_quota"`
	RealmQuota   string `toml:"realm_quota"`
	MaxFiles     int    `toml:"max_files"`
	// MaxRequest bounds one upload request.
	MaxRequest string `toml:"max_request"`
	// MaxUploads uploads (archive and secrets) may run at once on the
	// hub, MachineUploads per machine; MaxDownloads reads (indexes,
	// summaries, files, secrets) at once, MachineDownloads per machine.
	MaxUploads       int `toml:"max_uploads"`
	MachineUploads   int `toml:"machine_uploads"`
	MaxDownloads     int `toml:"max_downloads"`
	MachineDownloads int `toml:"machine_downloads"`
	// MaxRealms bounds the realms one machine may hold on the hub
	// (archive and secrets together).
	MaxRealms int `toml:"max_realms"`
	// MinFree is the free space the archive always leaves on the hub's
	// file system.
	MinFree string `toml:"min_free"`
}

// TerminalConfig governs the web app's terminal (work order C12): with
// Control the app offers to take control of a session's terminal and type
// into it. The machine must allow it too ([terminal] control in its
// config.toml). Off by default: terminals only observe.
type TerminalConfig struct {
	Control bool `toml:"control"`
}

// NtfyConfig names the ntfy server and topic notifications go to.
type NtfyConfig struct {
	URL   string `toml:"url"`
	Topic string `toml:"topic"`
	// Token is an access token (optional).
	Token string `toml:"token"`
}

// ConfigFile is the path of hub.toml.
func (l Layout) ConfigFile() string { return filepath.Join(l.Dir, "hub.toml") }

var topicRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// LoadConfig reads hub.toml (a missing file is an empty configuration).
func LoadConfig(l Layout) (*Config, error) {
	b, err := os.ReadFile(l.ConfigFile())
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	md, err := toml.Decode(string(b), &c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", l.ConfigFile(), err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("%s: unknown setting %q", l.ConfigFile(), und[0].String())
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", l.ConfigFile(), err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("public_url %q is not a URL", c.PublicURL)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return fmt.Errorf("public_url %q must be scheme://host[:port], without a path", c.PublicURL)
		}
		c.PublicURL = u.Scheme + "://" + u.Host
	}
	if _, err := c.Archive.Limits(); err != nil {
		return err
	}
	if n := c.Ntfy; n != nil {
		u, err := url.Parse(n.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("ntfy.url %q must be an http(s) URL", n.URL)
		}
		n.URL = strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
		if !topicRE.MatchString(n.Topic) {
			return fmt.Errorf("ntfy.topic must be 1 to 64 letters, digits, '_' or '-'")
		}
		if strings.ContainsAny(n.Token, "\r\n") {
			return errors.New("ntfy.token holds a line break")
		}
	}
	return nil
}

// checkPublicURL checks the public URL against the public name: https and
// the same host and port.
func checkPublicURL(publicURL, host, port string) error {
	u, err := url.Parse(publicURL)
	if err != nil {
		return err
	}
	if u.Scheme != "https" {
		return fmt.Errorf("public_url %q must be https", publicURL)
	}
	h, p := u.Hostname(), u.Port()
	if p == "443" {
		p = ""
	}
	want := port
	if want == "443" {
		want = ""
	}
	if !strings.EqualFold(h, host) || p != want {
		name := host
		if port != "" {
			name = net.JoinHostPort(host, port)
		}
		return fmt.Errorf("public_url %q does not name the hub's public name %s", publicURL, name)
	}
	return nil
}

// PublicURL is the hub's URL for links: public_url from hub.toml, else
// https://<public name> (as stored by 'devc hub init').
func PublicURL(l Layout, st *Store) (string, error) {
	cfg, err := LoadConfig(l)
	if err != nil {
		return "", err
	}
	name, err := st.Meta(metaPublicName)
	if err != nil {
		return "", err
	}
	if name == "" {
		if cfg.PublicURL != "" {
			return cfg.PublicURL, nil
		}
		return "", errors.New("the hub has no public name: set public_url in hub.toml or run 'devc hub init --public-name'")
	}
	host, port, err := splitPublicName(name)
	if err != nil {
		return "", err
	}
	if cfg.PublicURL != "" {
		if err := checkPublicURL(cfg.PublicURL, host, port); err != nil {
			return "", err
		}
		return cfg.PublicURL, nil
	}
	return (&Server{host: host, port: port}).origin(), nil
}
