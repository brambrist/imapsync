// Package config loads, validates and applies defaults to the synchronizer's
// YAML config.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so it can be read from YAML as a string like
// "5m", "30s", "10m". The stock yaml.v3 does not parse that format.
type Duration time.Duration

// UnmarshalYAML parses the string via time.ParseDuration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("expected a duration string (e.g. \"5m\"): %w", err)
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the value as a plain time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Transport types for a sync endpoint.
const (
	EndpointIMAP    = "imap"    // IMAP server with master access (default)
	EndpointMaildir = "maildir" // local Maildir/Maildir++ on disk
	EndpointEWS     = "ews"     // Exchange Web Services (SOAP), impersonation
	EndpointPST     = "pst"     // local PST/OST archive, read-only (import source)
)

// Server describes one sync endpoint.
//
//   - type: imap - Host/Port/MasterUser/MasterPass; impersonation via SASL PLAIN
//     with authzid (authcid+password is the master, authzid is the target user).
//   - type: maildir - Root: path template to the user's Maildir. Placeholders:
//     %u (whole user_a/user_b), %n (local part before @), %d (domain).
//   - type: pst - Root: path template to the user's PST/OST archive (same
//     placeholders). Read-only: it can only be a sync source (one-way import).
//   - type: ews - EWSUrl (or Host), MasterUser/MasterPass is a service account
//     with the ApplicationImpersonation role; impersonation via the SOAP header
//     ExchangeImpersonation (PrimarySmtpAddress = user_a/user_b). Auth: Basic.
type Server struct {
	Type       string `yaml:"type"` // "" | "imap" | "maildir" | "ews"
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	MasterUser string `yaml:"master_user"` // authcid for SASL PLAIN / Basic user for EWS
	MasterPass string `yaml:"master_pass"` // master account password
	Root       string `yaml:"root"`        // Maildir path template (type: maildir)
	EWSUrl     string `yaml:"ews_url"`     // full EWS URL (else https://<host>/EWS/Exchange.asmx)
}

// FolderPair is an explicit mapping of a folder name on server A to one on
// server B. The names may differ across servers ("Sent" / "Sent Items").
type FolderPair struct {
	A string `yaml:"a"`
	B string `yaml:"b"`
}

// User is one synchronized user: a logical name for logs and the addresses
// (authzid) on each server.
type User struct {
	Name  string `yaml:"name"`
	UserA string `yaml:"user_a"`
	UserB string `yaml:"user_b"`
}

// Sources for the user and folder lists.
const (
	SourceYAML   = "yaml"   // users/folders come from this same YAML
	SourceSQLite = "sqlite" // users/folders come from the local sqlite DB
)

// Sync directions.
const (
	DirectionBoth = "both"   // copy missing messages A->B and B->A (default)
	DirectionAToB = "a-to-b" // copy A->B only (e.g. import an archive into a server)
	DirectionBToA = "b-to-a" // copy B->A only
)

// Config is the root config.
type Config struct {
	ServerA Server `yaml:"server_a"`
	ServerB Server `yaml:"server_b"`

	// Source - where to take the user and folder-pair lists from: "yaml"
	// (default) or "sqlite". With "sqlite" the folders/users sections in YAML
	// are optional and ignored; data is read from the SQLitePath file.
	Source     string `yaml:"source"`
	SQLitePath string `yaml:"sqlite_path"`

	// Folders/Users are populated either from YAML or from sqlite (see Source).
	Folders []FolderPair `yaml:"folders"`
	Users   []User       `yaml:"users"`

	Workers        int      `yaml:"workers"`          // worker count, must be < number of users
	SyncInterval   Duration `yaml:"sync_interval"`    // pause between full cycles
	StatsInterval  Duration `yaml:"stats_interval"`   // summary stats period
	PerUserTimeout Duration `yaml:"per_user_timeout"` // per-user processing timeout
	DialTimeout    Duration `yaml:"dial_timeout"`     // TCP connect timeout
	IOTimeout      Duration `yaml:"io_timeout"`       // timeout for one IMAP operation (FETCH/APPEND/...)
	FetchBatchSize int      `yaml:"fetch_batch_size"` // FETCH batch size
	InsecureTLS    bool     `yaml:"insecure_tls"`     // do not verify the certificate (for tests)

	// ConnectRetries - how many times to retry connecting/reconnecting to a
	// server on a transient error (0 - no retries). RetryBackoff is the base
	// pause between attempts (grows exponentially).
	ConnectRetries int      `yaml:"connect_retries"`
	RetryBackoff   Duration `yaml:"retry_backoff"`

	// FullResyncEvery - how often, in state_cache mode, to do a full folder
	// rescan (endpoint cache reset) to catch drift. 0 - never.
	FullResyncEvery Duration `yaml:"full_resync_every"`

	// MaxFailStreak - after this many consecutive failed runs a user's sync is
	// stopped (until `imapsync db-resume-user` or db-forget-user). Requires a DB
	// (state_cache or source: sqlite). Negative - disabled.
	MaxFailStreak int `yaml:"max_fail_streak"`

	// HashHeader - name of the custom header the surrogate hash is written into
	// on APPEND, so already-copied messages are found on later passes.
	HashHeader string `yaml:"hash_header"`

	// StateCache enables incremental reconciliation: the ID list comes from
	// UID SEARCH, headers are fetched only for new messages, and parsing is
	// cached in sqlite (sqlite_path). Requires sqlite_path to be set.
	StateCache bool `yaml:"state_cache"`

	// Direction - which way to copy missing messages: "both" (default),
	// "a-to-b" or "b-to-a". A read-only endpoint (e.g. type: pst) also forces
	// the direction away from it regardless of this setting.
	Direction string `yaml:"direction"`
}

// defaults, applied to zero values after parsing.
const (
	defaultPort            = 993
	defaultWorkers         = 4
	defaultSyncInterval    = Duration(5 * time.Minute)
	defaultStatsInterval   = Duration(1 * time.Minute)
	defaultPerUserTimeout  = Duration(10 * time.Minute)
	defaultDialTimeout     = Duration(30 * time.Second)
	defaultIOTimeout       = Duration(5 * time.Minute)
	defaultFetchBatchSize  = 200
	defaultHashHeader      = "X-Imapsync-Hash"
	defaultConnectRetries  = 3
	defaultRetryBackoff    = Duration(5 * time.Second)
	defaultFullResyncEvery = Duration(24 * time.Hour)
	defaultMaxFailStreak   = 10
)

// Load reads the config from a file, applies defaults and validates it.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	cfg.applyDefaults()
	if err := cfg.validateBase(); err != nil {
		return nil, fmt.Errorf("validating config %s: %w", path, err)
	}
	// With source: yaml the lists must be valid already. With source: sqlite the
	// caller populates and checks them via ValidateEntities after loading from
	// the DB.
	if cfg.Source == SourceYAML {
		if err := cfg.ValidateEntities(); err != nil {
			return nil, fmt.Errorf("validating config %s: %w", path, err)
		}
	}
	return &cfg, nil
}

// LoadEntitiesOnly reads the YAML and returns a config with folders/users parsed
// but without validating the base fields or the source mode. Needed for the
// sqlite import command, where the YAML itself may be partial (lists only).
func LoadEntitiesOnly(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if len(cfg.Folders) == 0 && len(cfg.Users) == 0 {
		return nil, fmt.Errorf("%s has neither folders nor users to import", path)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.ServerA.Port == 0 {
		c.ServerA.Port = defaultPort
	}
	if c.ServerB.Port == 0 {
		c.ServerB.Port = defaultPort
	}
	if c.Workers == 0 {
		c.Workers = defaultWorkers
	}
	if c.SyncInterval == 0 {
		c.SyncInterval = defaultSyncInterval
	}
	if c.StatsInterval == 0 {
		c.StatsInterval = defaultStatsInterval
	}
	if c.PerUserTimeout == 0 {
		c.PerUserTimeout = defaultPerUserTimeout
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = defaultDialTimeout
	}
	if c.IOTimeout == 0 {
		c.IOTimeout = defaultIOTimeout
	}
	if c.FetchBatchSize == 0 {
		c.FetchBatchSize = defaultFetchBatchSize
	}
	if c.HashHeader == "" {
		c.HashHeader = defaultHashHeader
	}
	if c.Source == "" {
		c.Source = SourceYAML
	}
	if c.Direction == "" {
		c.Direction = DirectionBoth
	}
	if c.ConnectRetries == 0 {
		c.ConnectRetries = defaultConnectRetries
	}
	if c.RetryBackoff == 0 {
		c.RetryBackoff = defaultRetryBackoff
	}
	if c.FullResyncEvery == 0 {
		c.FullResyncEvery = defaultFullResyncEvery
	}
	if c.MaxFailStreak == 0 {
		c.MaxFailStreak = defaultMaxFailStreak
	}
}

// validateBase checks everything that does not depend on the list source:
// servers, timings, source mode.
func (c *Config) validateBase() error {
	if err := validateServer("server_a", c.ServerA); err != nil {
		return err
	}
	if err := validateServer("server_b", c.ServerB); err != nil {
		return err
	}

	switch c.Source {
	case SourceYAML:
	case SourceSQLite:
		if strings.TrimSpace(c.SQLitePath) == "" {
			return fmt.Errorf("source: sqlite - sqlite_path is required")
		}
	default:
		return fmt.Errorf("unknown source %q (allowed: %q, %q)", c.Source, SourceYAML, SourceSQLite)
	}

	if c.StateCache && strings.TrimSpace(c.SQLitePath) == "" {
		return fmt.Errorf("state_cache: true - sqlite_path is required")
	}

	switch c.Direction {
	case DirectionBoth, DirectionAToB, DirectionBToA:
	default:
		return fmt.Errorf("unknown direction %q (allowed: %q, %q, %q)",
			c.Direction, DirectionBoth, DirectionAToB, DirectionBToA)
	}

	// A read-only endpoint (currently only pst) can be a source but not a
	// target. Both sides read-only leaves nothing to do.
	if readOnlyType(c.ServerA.Type) && readOnlyType(c.ServerB.Type) {
		return fmt.Errorf("both endpoints are read-only (type %q) - nothing to sync", c.ServerA.Type)
	}
	if readOnlyType(c.ServerA.Type) && c.Direction == DirectionBToA {
		return fmt.Errorf("server_a is read-only (type %q) but direction is %q", c.ServerA.Type, DirectionBToA)
	}
	if readOnlyType(c.ServerB.Type) && c.Direction == DirectionAToB {
		return fmt.Errorf("server_b is read-only (type %q) but direction is %q", c.ServerB.Type, DirectionAToB)
	}

	if c.Workers < 1 {
		return fmt.Errorf("workers must be >= 1, got %d", c.Workers)
	}
	if c.FetchBatchSize < 1 {
		return fmt.Errorf("fetch_batch_size must be >= 1, got %d", c.FetchBatchSize)
	}
	return nil
}

// ValidateEntities checks the folder and user lists and their consistency with
// the worker count. Called from Load with source: yaml and manually after
// loading from sqlite.
func (c *Config) ValidateEntities() error {
	if len(c.Folders) == 0 {
		return fmt.Errorf("no folder pairs configured (folders)")
	}
	for i, f := range c.Folders {
		if strings.TrimSpace(f.A) == "" || strings.TrimSpace(f.B) == "" {
			return fmt.Errorf("folders[%d]: empty folder name (a=%q b=%q)", i, f.A, f.B)
		}
	}

	if len(c.Users) == 0 {
		return fmt.Errorf("no users configured (users)")
	}
	seen := make(map[string]struct{}, len(c.Users))
	for i, u := range c.Users {
		if strings.TrimSpace(u.Name) == "" {
			return fmt.Errorf("users[%d]: empty name field", i)
		}
		if strings.TrimSpace(u.UserA) == "" || strings.TrimSpace(u.UserB) == "" {
			return fmt.Errorf("users[%d] (%s): empty user_a or user_b", i, u.Name)
		}
		if _, dup := seen[u.Name]; dup {
			return fmt.Errorf("users[%d]: duplicate name %q", i, u.Name)
		}
		seen[u.Name] = struct{}{}
	}

	// Requirement from CLAUDE.md: fewer workers than users.
	if c.Workers >= len(c.Users) && len(c.Users) > 1 {
		return fmt.Errorf("workers (%d) must be less than the number of users (%d)", c.Workers, len(c.Users))
	}
	return nil
}

func validateServer(name string, s Server) error {
	switch s.Type {
	case "", EndpointIMAP:
		if strings.TrimSpace(s.Host) == "" {
			return fmt.Errorf("%s: host is not set", name)
		}
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("%s: invalid port %d", name, s.Port)
		}
		if strings.TrimSpace(s.MasterUser) == "" {
			return fmt.Errorf("%s: master_user is not set", name)
		}
		if strings.TrimSpace(s.MasterPass) == "" {
			return fmt.Errorf("%s: master_pass is not set", name)
		}
	case EndpointMaildir:
		if strings.TrimSpace(s.Root) == "" {
			return fmt.Errorf("%s: type maildir - root is not set (Maildir path template)", name)
		}
	case EndpointPST:
		if strings.TrimSpace(s.Root) == "" {
			return fmt.Errorf("%s: type pst - root is not set (PST/OST path template)", name)
		}
	case EndpointEWS:
		if strings.TrimSpace(s.EWSUrl) == "" && strings.TrimSpace(s.Host) == "" {
			return fmt.Errorf("%s: type ews - ews_url or host is required", name)
		}
		if strings.TrimSpace(s.MasterUser) == "" || strings.TrimSpace(s.MasterPass) == "" {
			return fmt.Errorf("%s: type ews - master_user/master_pass are required (service account with ApplicationImpersonation)", name)
		}
	default:
		return fmt.Errorf("%s: type %q is not supported (%q, %q, %q, %q)", name, s.Type, EndpointIMAP, EndpointMaildir, EndpointEWS, EndpointPST)
	}
	return nil
}

// readOnlyType reports whether an endpoint type can only be a sync source
// (Append is unsupported).
func readOnlyType(t string) bool { return t == EndpointPST }

// Addr returns a human-readable endpoint address for logs.
func (s Server) Addr() string {
	switch s.Type {
	case EndpointMaildir:
		return "maildir:" + s.Root
	case EndpointPST:
		return "pst:" + s.Root
	case EndpointEWS:
		if s.EWSUrl != "" {
			return s.EWSUrl
		}
		return "ews:" + s.Host
	}
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}
