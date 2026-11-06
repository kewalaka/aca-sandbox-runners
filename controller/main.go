package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("controller exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(osEnv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Managed identity on ACA (AZURE_CLIENT_ID selects the user-assigned one); az login locally.
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return fmt.Errorf("azure credential: %w", err)
	}
	sbx := newSandboxClient(cfg, cred)

	ssClient, err := newScaleSetClient(cfg)
	if err != nil {
		return fmt.Errorf("scale set client: %w", err)
	}
	// Repo-level scale sets live in the Default runner group (id 1); named groups are org/enterprise only.
	ss, err := ssClient.GetRunnerScaleSet(ctx, 1, cfg.ScaleSetName)
	if err != nil {
		return fmt.Errorf("get scale set: %w", err)
	}
	if ss == nil {
		ss, err = ssClient.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name: cfg.ScaleSetName, RunnerGroupID: 1,
			Labels:        []scaleset.Label{{Name: cfg.ScaleSetName}},
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return fmt.Errorf("create scale set: %w", err)
		}
	}
	ssClient.SetSystemInfo(systemInfo(ss.ID))
	log.Info("scale set ready", "stage", "startup", "scale_set_id", ss.ID, "name", ss.Name, "max_runners", cfg.MaxRunners, "warm_pool", cfg.WarmPool,
		"keep_on_failure", cfg.KeepOnFailure, "probe", cfg.ProbeRunnerOnline)

	scaler := newScaler(cfg, log.With("component", "scaler"), sbx, ssClient, ss.ID, imageResolver(cfg, sbx))
	scaler.probe = onlineProbe(cfg, log, func(n string) bool { _, ok := scaler.get(n); return ok })
	if err := scaler.Adopt(ctx); err != nil {
		return fmt.Errorf("adopt existing sandboxes: %w", err)
	}
	defer func() { scaler.Shutdown(context.WithoutCancel(ctx)); scaler.Wait() }()

	rec := &Reconciler{cfg: cfg, log: log.With("component", "reconciler"), sbx: sbx, scaler: scaler}
	go rec.Run(ctx)

	hostname, _ := os.Hostname()
	session, err := openSession(ctx, log, ssClient, ss.ID, hostname)
	if err != nil {
		return fmt.Errorf("message session: %w", err)
	}
	defer session.Close(context.WithoutCancel(ctx))

	l, err := listener.New(observedClient{Client: session, log: log.With("component", "listener")},
		listener.Config{ScaleSetID: ss.ID, MaxRunners: cfg.MaxRunners, Logger: log.With("component", "listener").WithGroup("lib")})
	if err != nil {
		return fmt.Errorf("listener: %w", err)
	}
	// A session loss is unrecoverable in-process: exit and let the platform restart (state is re-adopted).
	return l.Run(ctx, scaler)
}

// openSession retries while a crashed predecessor's session is still held server-side (409 Conflict).
func openSession(ctx context.Context, log *slog.Logger, c *scaleset.Client, id int, owner string) (*scaleset.MessageSessionClient, error) {
	started := time.Now()
	for {
		s, err := c.MessageSessionClient(ctx, id, owner)
		if err == nil {
			if waited := time.Since(started); waited > 5*time.Second {
				log.Info("session acquired after conflict", "stage", "startup", "waited_ms", waited.Milliseconds())
			}
			return s, nil
		}
		if !strings.Contains(err.Error(), "409") {
			return nil, err
		}
		log.Warn("scale set session held by another owner, retrying", "stage", "startup", "waited_ms", time.Since(started).Milliseconds())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func newScaleSetClient(cfg Config) (*scaleset.Client, error) {
	info := systemInfo(0)
	if cfg.Token != "" {
		return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL: cfg.ConfigURL, PersonalAccessToken: cfg.Token, SystemInfo: info,
		})
	}
	return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: cfg.ConfigURL, SystemInfo: info,
		GitHubAppAuth: scaleset.GitHubAppAuth{ClientID: cfg.AppClientID, InstallationID: cfg.AppInstallID, PrivateKey: cfg.AppPrivateKey},
	})
}

func systemInfo(scaleSetID int) scaleset.SystemInfo {
	return scaleset.SystemInfo{System: "aca-sandbox-controller", Subsystem: "controller", ScaleSetID: scaleSetID, Version: "dev"}
}
