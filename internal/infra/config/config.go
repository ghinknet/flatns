// Package config loads flatns configuration from config.yaml in the working
// directory and exposes it as a process-wide singleton. It owns its own SIGHUP
// listener: a goroutine started in Init reloads the file on every SIGHUP and
// fans the event out to the registered OnReload hooks, so callers never have to
// wire up signal handling themselves.
//
// Usage mirrors the rest of the infra layer:
//
//	config.Init()
//	defer config.Cleanup()
//	cfg := config.Get()
package config

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/viper"

	"flatns/internal/provider"
)

// Config is the top-level configuration document.
type Config struct {
	// Log configures logging output and rotation.
	Log LogConfig `mapstructure:"log"`
	// Instance is the default deployment identifier embedded in the remark of
	// every managed record. It lets several independent flatns deployments
	// (e.g. one per region, each pinned to a subset of IPs) flatten the same
	// source without claiming each other's records. Empty means the unnamed
	// instance, which keeps the legacy remark format. Per-entry Instance
	// overrides this default.
	Instance string `mapstructure:"instance"`
	// Resolvers is the default list of recursive DNS servers used for CNAME
	// resolution when an entry does not override them. Empty means use the
	// system resolver. Each entry is host or host:port (port defaults to 53).
	Resolvers []string `mapstructure:"resolvers"`
	// Providers maps a user-defined provider alias to its credentials. Entries
	// reference a provider by this alias so the same vendor account can be
	// reused across many flatten entries.
	Providers map[string]ProviderConfig `mapstructure:"providers"`
	// Flattens is the list of flattening jobs, each handled by its own worker.
	Flattens []FlattenConfig `mapstructure:"flattens"`
}

// LogConfig configures the zap logger and its lumberjack-backed file rotation.
type LogConfig struct {
	// Debug enables debug-level logging when true; otherwise info level is used.
	Debug bool `mapstructure:"debug"`
	// File holds the output file paths for the combined and error-only logs.
	File struct {
		// All receives every log line at the configured level. Empty disables
		// the combined file sink (stdout is always written).
		All string `mapstructure:"all"`
		// Err receives only Warn and above. Empty disables the error file sink.
		Err string `mapstructure:"err"`
	} `mapstructure:"file"`
	// MaxSize is the per-file size threshold for rotation, in megabytes.
	MaxSize int `mapstructure:"max_size"`
	// MaxBackups is the maximum number of rotated files to retain.
	MaxBackups int `mapstructure:"max_backups"`
	// MaxAge is the maximum age of a rotated file, in days.
	MaxAge int `mapstructure:"max_age"`
	// Compress enables gzip compression of rotated files.
	Compress bool `mapstructure:"compress"`
}

// ProviderConfig holds the credentials for one provider alias.
type ProviderConfig struct {
	// Type selects the registered provider implementation (tencent, aliyun, ...).
	Type string `mapstructure:"type"`
	// SecretID is the API key id (Tencent SecretId / Aliyun AccessKeyId).
	SecretID string `mapstructure:"secret_id"`
	// SecretKey is the API secret.
	SecretKey string `mapstructure:"secret_key"`
	// Region is an optional region hint passed to the SDK.
	Region string `mapstructure:"region"`
	// Extra carries provider-specific options.
	Extra map[string]string `mapstructure:"extra"`
}

// FlattenConfig describes a single CNAME-flattening job.
type FlattenConfig struct {
	// Name is a human-friendly identifier used in logs.
	Name string `mapstructure:"name"`
	// Instance overrides the global Config.Instance for this entry only. Empty
	// inherits the global value.
	Instance string `mapstructure:"instance"`
	// Provider references a key in Config.Providers.
	Provider string `mapstructure:"provider"`
	// Source is the CNAME to resolve, e.g. "example.cn.eo.dnse1.com".
	Source string `mapstructure:"source"`
	// Domain is the target registrable zone, e.g. "example.cn".
	Domain string `mapstructure:"domain"`
	// SubDomain is the target host within Domain; "@" denotes the apex.
	SubDomain string `mapstructure:"sub_domain"`
	// TTL is the TTL applied to managed records, in seconds.
	TTL uint64 `mapstructure:"ttl"`
	// Interval is the polling interval for this entry.
	Interval time.Duration `mapstructure:"interval"`
	// IPv4 controls management of A records. A nil value means enabled for
	// backwards compatibility with configurations written before this option.
	IPv4 *bool `mapstructure:"ipv4"`
	// IPv6 controls management of AAAA records.
	IPv6 bool `mapstructure:"ipv6"`
	// MaxRecords caps how many managed records are kept per record type (A and
	// AAAA are limited independently). Some providers' free tiers restrict the
	// number of values a single sub-domain may resolve to (e.g. DNSPod allows
	// only a couple per type); flattening a source with more addresses than that
	// would otherwise make the provider reject the surplus CreateRecord calls.
	// When the source resolves to more addresses than this limit, the first
	// MaxRecords (sorted) are kept and the rest are dropped. 0 means unlimited.
	MaxRecords int `mapstructure:"max_records"`
	// MaxRecordsTotal caps the combined number of managed records across both
	// types, for providers whose quota counts A and AAAA together. When the cap
	// bites, the budget is split evenly between the enabled types and either
	// type's unused share flows to the other. 0 means unlimited. Both limits may
	// be set; each is enforced independently.
	MaxRecordsTotal int `mapstructure:"max_records_total"`
	// Resolvers overrides the global Resolvers list for this entry only.
	Resolvers []string `mapstructure:"resolvers"`
}

// ToProviderConfig converts the stored credentials into the provider package's
// generic Config.
func (p ProviderConfig) ToProviderConfig() provider.Config {
	return provider.Config{
		Name:      p.Type,
		SecretID:  p.SecretID,
		SecretKey: p.SecretKey,
		Region:    p.Region,
		Extra:     p.Extra,
	}
}

// defaults supplies sane fallbacks for optional fields.
const (
	defaultTTL       = 600
	defaultInterval  = 5 * time.Minute
	defaultSubDomain = "@"
)

// Validate checks the configuration for internal consistency and fills in
// defaults. It returns the first validation error encountered.
func (c *Config) Validate() error {
	if len(c.Flattens) == 0 {
		return fmt.Errorf("config: at least one flatten entry is required")
	}
	if err := validateResolvers(c.Resolvers); err != nil {
		return fmt.Errorf("config: global resolvers: %w", err)
	}

	seen := make(map[string]struct{}, len(c.Flattens))
	for i := range c.Flattens {
		f := &c.Flattens[i]
		if f.Name == "" {
			f.Name = fmt.Sprintf("flatten-%d", i)
		}
		if _, dup := seen[f.Name]; dup {
			return fmt.Errorf("config: duplicate flatten name %q", f.Name)
		}
		seen[f.Name] = struct{}{}

		if f.Source == "" {
			return fmt.Errorf("config: flatten %q: source is required", f.Name)
		}
		if f.Domain == "" {
			return fmt.Errorf("config: flatten %q: domain is required", f.Name)
		}
		if f.SubDomain == "" {
			f.SubDomain = defaultSubDomain
		}
		if f.Provider == "" {
			return fmt.Errorf("config: flatten %q: provider is required", f.Name)
		}
		if _, ok := c.Providers[f.Provider]; !ok {
			return fmt.Errorf("config: flatten %q: provider alias %q not defined", f.Name, f.Provider)
		}
		if f.TTL == 0 {
			f.TTL = defaultTTL
		}
		if f.Interval <= 0 {
			f.Interval = defaultInterval
		}
		if f.IPv4 == nil {
			enabled := true
			f.IPv4 = &enabled
		}
		if !*f.IPv4 && !f.IPv6 {
			return fmt.Errorf("config: flatten %q: at least one of ipv4 or ipv6 must be enabled", f.Name)
		}
		if f.MaxRecords < 0 {
			return fmt.Errorf("config: flatten %q: max_records must not be negative", f.Name)
		}
		if f.MaxRecordsTotal < 0 {
			return fmt.Errorf("config: flatten %q: max_records_total must not be negative", f.Name)
		}
		if err := validateResolvers(f.Resolvers); err != nil {
			return fmt.Errorf("config: flatten %q resolvers: %w", f.Name, err)
		}
		// Fall back to global resolvers when the entry does not specify any.
		if len(f.Resolvers) == 0 {
			f.Resolvers = c.Resolvers
		}
		// Inherit the global instance when the entry does not override it.
		if f.Instance == "" {
			f.Instance = c.Instance
		}
	}

	for alias, p := range c.Providers {
		if p.Type == "" {
			return fmt.Errorf("config: provider %q: type is required", alias)
		}
	}
	return nil
}

// validateResolvers ensures every resolver entry is a parseable host or
// host:port pair.
func validateResolvers(resolvers []string) error {
	for _, r := range resolvers {
		if r == "" {
			return fmt.Errorf("empty resolver entry")
		}
		// Accept bare host (no port) by attempting to split; if SplitHostPort
		// fails we treat the whole string as a host and validate it loosely.
		if _, _, err := net.SplitHostPort(r); err != nil {
			if strings.Contains(r, ":") && !strings.Contains(r, "]") {
				// Looks like a malformed host:port rather than a bare IPv6.
				return fmt.Errorf("invalid resolver %q: %w", r, err)
			}
		}
	}
	return nil
}

// configName / configType / configPath define where the file is looked up.
// Following the rest of the infra layer, the path is fixed (config.yaml in the
// working directory) rather than configurable via flags.
const (
	configName = "config"
	configType = "yaml"
	configPath = "./"
)

// Package-level singleton state. The configuration is small and read far more
// often than it changes, so a single RWMutex-guarded pointer is sufficient.
var (
	mu       sync.RWMutex
	current  *Config
	onReload []func()

	// signalStopChan stops the SIGHUP listener goroutine on Cleanup.
	signalStopChan chan struct{}
)

// load reads and validates config.yaml, returning the parsed Config. It does
// not touch the singleton; callers decide whether to publish the result.
func load() (*Config, error) {
	v := viper.New()
	v.SetConfigName(configName)
	v.SetConfigType(configType)
	v.AddConfigPath(configPath)

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read: %w", err)
	}
	var cfg Config
	if err := v.Unmarshal(&cfg, viper.DecodeHook(durationHook())); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// publish atomically swaps the cached config and refreshes the Debug snapshot.
func publish(cfg *Config) {
	mu.Lock()
	current = cfg
	mu.Unlock()
}

// Init loads config.yaml, populates the singleton, and starts the SIGHUP
// listener goroutine. It is fatal on the initial load failure since the process
// cannot run without a valid configuration. Init must be called once at
// start-up before Get.
func Init() {
	cfg, err := load()
	if err != nil {
		log.Fatal("load config failed: ", err)
	}
	publish(cfg)

	// Listen for SIGHUP and reload in the background, so callers never wire up
	// signal handling themselves.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP)
	signalStopChan = make(chan struct{})

	go func() {
		for {
			select {
			case sig := <-sigChan:
				if sig == syscall.SIGHUP {
					reload()
				}
			case <-signalStopChan:
				signal.Stop(sigChan)
				return
			}
		}
	}()
}

// reload re-reads config.yaml and, on success, publishes it and fires the
// reload hooks so subscribers (logger, workers) rebuild. A bad edit is logged
// and ignored, leaving the previously loaded config running.
func reload() {
	cfg, err := load()
	if err != nil {
		log.Println("reload config failed, keeping previous config:", err)
		return
	}
	publish(cfg)
	fireReload()
}

// Get returns the most recently loaded configuration. The returned pointer must
// be treated as read-only.
func Get() *Config {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// OnReload registers a callback invoked after every successful SIGHUP reload.
// Callbacks run synchronously in registration order.
func OnReload(fn func()) {
	mu.Lock()
	defer mu.Unlock()
	onReload = append(onReload, fn)
}

// fireReload invokes every registered hook under a snapshot of the slice so a
// hook may safely register further hooks without deadlocking.
func fireReload() {
	mu.RLock()
	hooks := make([]func(), len(onReload))
	copy(hooks, onReload)
	mu.RUnlock()
	for _, fn := range hooks {
		fn()
	}
}

// Cleanup stops the SIGHUP listener goroutine. It is safe to call when Init was
// never invoked.
func Cleanup() {
	if signalStopChan != nil {
		close(signalStopChan)
		signalStopChan = nil
	}
}
