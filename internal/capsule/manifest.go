// Package capsule is the Luna Capsules runtime (Capsules Spec): packages,
// the control protocol, supervision and links. OS isolation lives in backends.
package capsule

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const SpecVersion = "0.1"

type Manifest struct {
	Spec    string            `toml:"spec"`
	ID      string            `toml:"id"`
	Version string            `toml:"version"`
	Runtime string            `toml:"runtime"`
	Entry   string            `toml:"entry"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
	Limits  struct {
		Memory Size    `toml:"memory"`
		CPU    float64 `toml:"cpu"`
		Pids   int     `toml:"pids"`
		Files  int     `toml:"files"`
	} `toml:"limits"`
	FS struct {
		Writable  string `toml:"writable"`
		DataQuota Size   `toml:"data_quota"`
	} `toml:"fs"`
	Net     Net      `toml:"net"`
	Devices []string `toml:"devices"`
	Links   Links    `toml:"links"`
	Health  struct {
		Interval Duration `toml:"interval"`
		Timeout  Duration `toml:"timeout"`
	} `toml:"health"`
	Restart string `toml:"restart"`
}

type Links struct {
	Provides []string `toml:"provides"`
	Uses     []string `toml:"uses"`
}

// Size is a byte count written as "64m", "512k", "1g" or plain bytes.
type Size int64

func (s *Size) UnmarshalText(b []byte) error {
	t := strings.ToLower(strings.TrimSpace(string(b)))
	mult := int64(1)
	if n := len(t); n > 0 {
		switch t[n-1] {
		case 'k':
			mult, t = 1<<10, t[:n-1]
		case 'm':
			mult, t = 1<<20, t[:n-1]
		case 'g':
			mult, t = 1<<30, t[:n-1]
		}
	}
	v, err := strconv.ParseInt(t, 10, 64)
	if err != nil || v <= 0 || v > 1<<40/mult {
		return fmt.Errorf("bad size %q (want e.g. 64m)", b)
	}
	*s = Size(v * mult)
	return nil
}

type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	*d = Duration(v)
	return err
}

// Net is "none" (nil) or an allowlist of host:port.
type Net []string

func (n *Net) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case string:
		if v != "none" {
			return fmt.Errorf(`net must be "none" or a list of host:port, got %q`, v)
		}
		*n = nil
	case []any:
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return errors.New("net entries must be strings")
			}
			*n = append(*n, s)
		}
	default:
		return errors.New(`net must be "none" or a list of host:port`)
	}
	return nil
}

var (
	idRe      = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)*$`)
	semverRe  = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
	linkRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	runtimeRe = regexp.MustCompile(`^(native|wasm|luna|[a-z]+-\d+(\.\d+)?)$`)
	envRe     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
)

// SplitUse splits a links.uses entry "<provider id>:<link>".
func SplitUse(u string) (id, name string, ok bool) {
	i := strings.LastIndexByte(u, ':')
	if i < 0 {
		return "", "", false
	}
	id, name = u[:i], u[i+1:]
	return id, name, idRe.MatchString(id) && linkRe.MatchString(name)
}

func (m *Manifest) defaults() {
	m.Limits.Memory, m.Limits.CPU, m.Limits.Pids, m.Limits.Files = 64<<20, 0.25, 32, 256
	m.FS.Writable, m.FS.DataQuota = "data", 32<<20
	m.Health.Interval, m.Health.Timeout = Duration(5*time.Second), Duration(2*time.Second)
	m.Restart = "always"
}

func (m *Manifest) Validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if m.Spec != SpecVersion {
		bad("spec must be %q, got %q", SpecVersion, m.Spec)
	}
	if len(m.ID) > 128 || !idRe.MatchString(m.ID) || !strings.Contains(m.ID, ".") {
		bad("id must be reverse-DNS like dev.example.plugin ([a-z0-9.-], ≤128 chars), got %q", m.ID)
	}
	if !semverRe.MatchString(m.Version) {
		bad("version must be semver like 1.2.3, got %q", m.Version)
	}
	if !runtimeRe.MatchString(m.Runtime) {
		bad("runtime must be native, wasm, luna or a shared runtime like python-3.12, got %q", m.Runtime)
	}
	if !localPath(m.Entry) {
		bad("entry must be a relative path inside the package, got %q", m.Entry)
	}
	for k := range m.Env {
		if !envRe.MatchString(k) || strings.HasPrefix(k, "CAPSULE_") {
			bad("env key %q must be UPPER_SNAKE and not start with CAPSULE_", k)
		}
	}
	if m.Limits.Memory < 4<<20 {
		bad("limits.memory must be at least 4m")
	}
	if m.Limits.CPU <= 0 || m.Limits.CPU > 64 {
		bad("limits.cpu must be between 0 and 64, got %v", m.Limits.CPU)
	}
	if m.Limits.Pids < 1 || m.Limits.Pids > 4096 {
		bad("limits.pids must be 1-4096, got %d", m.Limits.Pids)
	}
	if m.Limits.Files < 16 || m.Limits.Files > 65536 {
		bad("limits.files must be 16-65536, got %d", m.Limits.Files)
	}
	if m.FS.Writable != "none" && m.FS.Writable != "data" {
		bad(`fs.writable must be "none" or "data", got %q`, m.FS.Writable)
	}
	for _, hp := range m.Net {
		if h, p, ok := strings.Cut(hp, ":"); !ok || h == "" || p == "" {
			bad("net entry %q must be host:port", hp)
		}
	}
	for _, d := range m.Devices {
		switch d {
		case "bluetooth", "serial", "gpu", "camera":
		default:
			bad("unknown device class %q", d)
		}
	}
	for _, l := range m.Links.Provides {
		if !linkRe.MatchString(l) {
			bad("links.provides entry %q must match [a-z0-9_-]", l)
		}
	}
	for _, u := range m.Links.Uses {
		if _, _, ok := SplitUse(u); !ok {
			bad("links.uses entry %q must be <provider id>:<link>", u)
		}
	}
	if m.Health.Interval < Duration(time.Second) || m.Health.Timeout <= 0 || m.Health.Timeout >= m.Health.Interval {
		bad("health.interval must be ≥1s and health.timeout shorter than it")
	}
	switch m.Restart {
	case "always", "on-failure", "never":
	default:
		bad(`restart must be "always", "on-failure" or "never", got %q`, m.Restart)
	}
	return errors.Join(errs...)
}

// localPath reports whether p is a clean relative slash path that stays inside its root.
func localPath(p string) bool {
	return p != "" && !strings.Contains(p, `\`) && path.Clean(p) == p && filepath.IsLocal(p)
}

// LoadManifest reads capsule.toml, or the [capsule] table of a merged plugin.toml
// (where id, version and [links] may sit at the top level).
func LoadManifest(dir string) (*Manifest, error) {
	m := &Manifest{}
	m.defaults()
	if b, err := os.ReadFile(filepath.Join(dir, "capsule.toml")); err == nil {
		md, err := toml.Decode(string(b), m)
		if err != nil {
			return nil, fmt.Errorf("capsule.toml: %w", err)
		}
		if und := md.Undecoded(); len(und) > 0 {
			return nil, fmt.Errorf("capsule.toml: unknown keys %v", und)
		}
		return m, m.Validate()
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "plugin.toml"))
	if err != nil {
		return nil, errors.New("no capsule.toml or plugin.toml in package")
	}
	var merged struct {
		ID      string         `toml:"id"`
		Version string         `toml:"version"`
		Links   Links          `toml:"links"`
		Capsule toml.Primitive `toml:"capsule"`
	}
	md, err := toml.Decode(string(b), &merged)
	if err != nil {
		return nil, fmt.Errorf("plugin.toml: %w", err)
	}
	if !md.IsDefined("capsule") {
		return nil, errors.New("plugin.toml has no [capsule] table")
	}
	m.Spec, m.ID, m.Version, m.Links = SpecVersion, merged.ID, merged.Version, merged.Links
	if err := md.PrimitiveDecode(merged.Capsule, m); err != nil {
		return nil, fmt.Errorf("plugin.toml [capsule]: %w", err)
	}
	for _, k := range md.Undecoded() {
		if len(k) > 1 && k[0] == "capsule" {
			return nil, fmt.Errorf("plugin.toml: unknown key %s", k)
		}
	}
	return m, m.Validate()
}
