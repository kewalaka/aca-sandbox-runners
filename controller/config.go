package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is loaded from environment variables only (no files, no flags) so it maps 1:1 onto container app env/secrets.
type Config struct {
	// GitHub: repository-level scale set registration.
	ConfigURL        string
	Token            string // Optional PAT instead of GitHub App authentication
	AppClientID      string
	AppInstallID     int64
	AppPrivateKey    string
	ScaleSetName     string // also the runs-on label
	MaxRunners       int
	WarmPool         int // idle runners kept registered ahead of demand
	CreateConcurrent int

	// Sandbox data plane.
	Endpoint        string
	SubscriptionID  string
	ResourceGroup   string
	Group           string
	DiskImageID     string // empty: newest Ready disk image whose name label starts with DiskImagePrefix
	DiskImagePrefix string
	CPU             string
	Memory          string
	EgressAllow     []string // empty: no egress policy (allow all)

	// Disposal backstops.
	AutoSuspend    time.Duration
	AutoDelete     time.Duration
	MaxSandboxAge  time.Duration
	ReconcileEvery time.Duration
	OrphanGrace    time.Duration

	// Failure forensics.
	KeepOnFailure   bool
	SnapshotResults []string
	SnapshotTTL     time.Duration

	ProbeRunnerOnline bool // poll the GitHub.com REST API (PAT only) to timestamp "runner online"
	LogLevel          string
}

var defaultEgress = []string{"*.actions.githubusercontent.com", "*.github.com", "github.com"}

func loadConfig(env func(string) string) (Config, error) {
	var errs []error
	str := func(key, def string) string {
		if v := env(key); v != "" {
			return v
		}
		return def
	}
	num := func(key string, def int) int {
		v := env(key)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
		return n
	}
	dur := func(key string, def time.Duration) time.Duration {
		v := env(key)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
		return d
	}
	list := func(key string, def []string) []string {
		v := env(key)
		if v == "" {
			return def
		}
		var out []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}

	endpoint := env("SANDBOX_ENDPOINT")
	if endpoint == "" && env("SANDBOX_REGION") != "" {
		endpoint = "https://management." + env("SANDBOX_REGION") + ".azuredevcompute.io"
	}
	maxJob := dur("MAX_JOB_DURATION", 6*time.Hour)
	c := Config{
		ConfigURL:        env("GITHUB_CONFIG_URL"),
		Token:            env("GITHUB_TOKEN"),
		AppClientID:      env("GITHUB_APP_CLIENT_ID"),
		AppInstallID:     int64(num("GITHUB_APP_INSTALLATION_ID", 0)),
		AppPrivateKey:    env("GITHUB_APP_PRIVATE_KEY"),
		ScaleSetName:     str("SCALE_SET_NAME", "aca-sandbox"),
		MaxRunners:       num("MAX_RUNNERS", 10),
		WarmPool:         num("WARM_POOL", 0),
		CreateConcurrent: num("CREATE_CONCURRENCY", 8),

		Endpoint:        endpoint,
		SubscriptionID:  env("SANDBOX_SUBSCRIPTION_ID"),
		ResourceGroup:   env("SANDBOX_RESOURCE_GROUP"),
		Group:           env("SANDBOX_GROUP"),
		DiskImageID:     env("SANDBOX_DISK_IMAGE_ID"),
		DiskImagePrefix: str("SANDBOX_DISK_IMAGE_PREFIX", "runner-"),
		CPU:             str("SANDBOX_CPU", "2000m"),
		Memory:          str("SANDBOX_MEMORY", "4096Mi"),

		// Backstops. The platform's idle clock only counts data-plane calls (exec), not the runner's outbound traffic, CPU or
		// disk, so a running job looks idle. Every reaper must therefore outlive the longest job: GitHub's default
		// timeout-minutes is 360 (self-hosted allows more; raise MAX_JOB_DURATION with it). Delete after suspend is coarse
		// and late, hence the reconciler.
		AutoSuspend:    dur("AUTO_SUSPEND", maxJob+15*time.Minute),
		AutoDelete:     dur("AUTO_DELETE", 5*time.Minute),
		MaxSandboxAge:  dur("MAX_SANDBOX_AGE", maxJob+10*time.Minute),
		ReconcileEvery: dur("RECONCILE_INTERVAL", 2*time.Minute),
		OrphanGrace:    dur("ORPHAN_GRACE", 5*time.Minute),

		KeepOnFailure:   str("KEEP_ON_FAILURE", "false") == "true",
		SnapshotResults: list("SNAPSHOT_RESULTS", []string{"failed"}),
		SnapshotTTL:     dur("SNAPSHOT_TTL", 24*time.Hour),

		LogLevel: str("LOG_LEVEL", "info"),
	}
	configURL, _ := url.Parse(c.ConfigURL)
	// The optional REST measurement uses api.github.com; never send another host's PAT there.
	c.ProbeRunnerOnline = str("PROBE_RUNNER_ONLINE", "true") == "true" && c.Token != "" && configURL != nil && configURL.Hostname() == "github.com"

	if egress := list("EGRESS_ALLOW", defaultEgress); len(egress) == 1 && egress[0] == "*" {
		c.EgressAllow = nil
	} else {
		c.EgressAllow = egress
	}

	for k, v := range map[string]string{
		"GITHUB_CONFIG_URL": c.ConfigURL, "SANDBOX_SUBSCRIPTION_ID": c.SubscriptionID,
		"SANDBOX_RESOURCE_GROUP": c.ResourceGroup, "SANDBOX_GROUP": c.Group,
		"SANDBOX_ENDPOINT (or SANDBOX_REGION)": c.Endpoint,
	} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", k))
		}
	}
	if c.Token == "" && (c.AppClientID == "" || c.AppInstallID == 0 || c.AppPrivateKey == "") {
		errs = append(errs, errors.New("set GITHUB_TOKEN or GITHUB_APP_CLIENT_ID + GITHUB_APP_INSTALLATION_ID + GITHUB_APP_PRIVATE_KEY"))
	}
	if c.WarmPool > c.MaxRunners {
		errs = append(errs, errors.New("WARM_POOL must not exceed MAX_RUNNERS"))
	}
	if c.AutoSuspend <= maxJob || c.MaxSandboxAge <= maxJob {
		errs = append(errs, errors.New("AUTO_SUSPEND and MAX_SANDBOX_AGE must exceed MAX_JOB_DURATION or long jobs are reaped mid-run"))
	}
	return c, errors.Join(errs...)
}

// ownerRepo extracts owner/repo from the config URL for the REST probe.
func (c Config) ownerRepo() (string, string, bool) {
	u, err := url.Parse(c.ConfigURL)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func osEnv(key string) string { return os.Getenv(key) }
