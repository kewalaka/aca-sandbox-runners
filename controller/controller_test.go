package main

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

type fakeSandboxes struct {
	mu        sync.Mutex
	sandboxes map[string]Sandbox
	snapshots []Snapshot
	next      int
	createErr error
}

func newFakeSandboxes() *fakeSandboxes { return &fakeSandboxes{sandboxes: map[string]Sandbox{}} }

func (f *fakeSandboxes) CreateSandbox(_ context.Context, r CreateSandboxRequest) (*Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.next++
	sb := Sandbox{ID: "sbx" + strconv.Itoa(f.next), State: "Running", Labels: r.Labels, CreatedAt: time.Now()}
	f.sandboxes[sb.ID] = sb
	return &sb, nil
}
func (f *fakeSandboxes) ListSandboxes(context.Context) (out []Sandbox, _ error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sandboxes {
		out = append(out, s)
	}
	return
}
func (f *fakeSandboxes) DeleteSandbox(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sandboxes, id)
	return nil
}
func (f *fakeSandboxes) Snapshot(_ context.Context, id string, l map[string]string) (*Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := Snapshot{ID: "snap-" + id, Labels: l, CreatedAt: time.Now()}
	f.snapshots = append(f.snapshots, s)
	return &s, nil
}
func (f *fakeSandboxes) ListSnapshots(context.Context) ([]Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Snapshot(nil), f.snapshots...), nil
}
func (f *fakeSandboxes) DeleteSnapshot(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.snapshots {
		if s.ID == id {
			f.snapshots = append(f.snapshots[:i], f.snapshots[i+1:]...)
		}
	}
	return nil
}
func (f *fakeSandboxes) ListDiskImages(context.Context) ([]DiskImage, error) { return nil, nil }

type fakeScaleSet struct {
	mu      sync.Mutex
	next    int
	removed []int64
}

func (f *fakeScaleSet) GenerateJitRunnerConfig(_ context.Context, _ *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: f.next}, EncodedJITConfig: "jit"}, nil
}
func (f *fakeScaleSet) RemoveRunner(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func testScaler(cfg Config) (*Scaler, *fakeSandboxes, *fakeScaleSet) {
	sbx, ss := newFakeSandboxes(), &fakeScaleSet{}
	cfg.ScaleSetName = "aca-sandbox"
	cfg.CreateConcurrent = 4
	s := newScaler(cfg, slog.New(slog.DiscardHandler), sbx, ss, 7, func(context.Context) (string, error) { return "img", nil })
	return s, sbx, ss
}

func TestDesiredCountScalesWithDemandAndCaps(t *testing.T) {
	s, sbx, _ := testScaler(Config{MaxRunners: 3, WarmPool: 1})
	ctx := context.Background()

	for _, tc := range []struct{ assigned, want int }{{0, 1}, {1, 2}, {5, 3}, {0, 3}} {
		got, err := s.HandleDesiredRunnerCount(ctx, tc.assigned)
		if err != nil || got != tc.want {
			t.Fatalf("assigned=%d: got %d err=%v, want %d", tc.assigned, got, err, tc.want)
		}
	}
	if len(sbx.sandboxes) != 3 {
		t.Fatalf("expected 3 sandboxes, got %d", len(sbx.sandboxes))
	}
}

func TestCreateFailureDoesNotLeakRunner(t *testing.T) {
	s, sbx, ss := testScaler(Config{MaxRunners: 2})
	sbx.createErr = errors.New("quota")
	if got, err := s.HandleDesiredRunnerCount(context.Background(), 2); err != nil || got != 0 {
		t.Fatalf("got %d err=%v, want 0 nil (failures are logged and retried)", got, err)
	}
	if len(ss.removed) != 2 {
		t.Fatalf("expected both minted runners removed, got %v", ss.removed)
	}
}

func TestJobCompletedDeletesSandboxAndSnapshotsFailures(t *testing.T) {
	s, sbx, ss := testScaler(Config{MaxRunners: 5, KeepOnFailure: true, SnapshotResults: []string{"failed"}})
	ctx := context.Background()
	if _, err := s.HandleDesiredRunnerCount(ctx, 3); err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range s.runners {
		names = append(names, n)
	}
	complete := func(name, result string) {
		if err := s.HandleJobCompleted(ctx, &scaleset.JobCompleted{Result: result, RunnerName: name}); err != nil {
			t.Fatal(err)
		}
	}
	complete(names[0], "succeeded")
	complete(names[1], "failed")
	complete(names[2], "canceled")
	s.Wait()

	if len(sbx.sandboxes) != 0 || s.count() != 0 {
		t.Fatalf("sandboxes=%d tracked=%d, want 0/0", len(sbx.sandboxes), s.count())
	}
	if len(sbx.snapshots) != 1 || sbx.snapshots[0].Labels["result"] != "failed" {
		t.Fatalf("expected exactly one snapshot for the failed job, got %+v", sbx.snapshots)
	}
	if len(ss.removed) != 3 {
		t.Fatalf("expected runner removal for all, got %v", ss.removed)
	}
}

func TestAdoptAndSweep(t *testing.T) {
	cfg := Config{MaxRunners: 5, MaxSandboxAge: time.Hour, OrphanGrace: time.Minute, SnapshotTTL: time.Hour}
	s, sbx, _ := testScaler(cfg)
	lbl := func(n string) map[string]string { return s.sandboxLabels(n, 1) }
	sbx.sandboxes["fresh"] = Sandbox{ID: "fresh", State: "Running", Labels: lbl("a"), CreatedAt: time.Now()}
	sbx.sandboxes["old"] = Sandbox{ID: "old", State: "Running", Labels: lbl("b"), CreatedAt: time.Now().Add(-2 * time.Hour)}
	sbx.sandboxes["dead"] = Sandbox{ID: "dead", State: "Stopped", Labels: lbl("c"), CreatedAt: time.Now().Add(-5 * time.Minute)}
	sbx.sandboxes["foreign"] = Sandbox{ID: "foreign", State: "Running", Labels: map[string]string{"purpose": "other"}, CreatedAt: time.Now().Add(-9 * time.Hour)}
	sbx.snapshots = []Snapshot{
		{ID: "snap-old", Labels: map[string]string{labelPurpose: purposeFailed}, CreatedAt: time.Now().Add(-2 * time.Hour)},
		{ID: "snap-new", Labels: map[string]string{labelPurpose: purposeFailed}, CreatedAt: time.Now()},
	}

	if err := s.Adopt(context.Background()); err != nil || s.count() != 3 {
		t.Fatalf("adopt: count=%d err=%v, want 3", s.count(), err)
	}
	(&Reconciler{cfg: cfg, log: slog.New(slog.DiscardHandler), sbx: sbx, scaler: s}).Sweep(context.Background())
	s.Wait()

	if _, ok := sbx.sandboxes["fresh"]; !ok || len(sbx.sandboxes) != 2 {
		t.Fatalf("expected only fresh + foreign to survive, got %v", sbx.sandboxes)
	}
	if len(sbx.snapshots) != 1 || sbx.snapshots[0].ID != "snap-new" {
		t.Fatalf("snapshot TTL sweep wrong: %+v", sbx.snapshots)
	}
}

func TestSweepPrunesRunnersWhoseSandboxVanished(t *testing.T) {
	cfg := Config{MaxRunners: 5, MaxSandboxAge: time.Hour, OrphanGrace: time.Minute, SnapshotTTL: time.Hour}
	s, sbx, _ := testScaler(cfg)
	s.track(&runner{name: "ghost", sandboxID: "gone", createdAt: time.Now().Add(-5 * time.Minute)})
	s.track(&runner{name: "new", sandboxID: "not-listed-yet", createdAt: time.Now()})
	(&Reconciler{cfg: cfg, log: slog.New(slog.DiscardHandler), sbx: sbx, scaler: s}).Sweep(context.Background())
	if _, ok := s.get("ghost"); ok {
		t.Fatal("runner with a vanished sandbox should be pruned")
	}
	if _, ok := s.get("new"); !ok {
		t.Fatal("recently created runner must survive list lag")
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	env := map[string]string{
		"GITHUB_CONFIG_URL": "https://github.com/o/r", "GITHUB_TOKEN": "t", "SANDBOX_SUBSCRIPTION_ID": "s",
		"SANDBOX_RESOURCE_GROUP": "rg", "SANDBOX_GROUP": "g", "EGRESS_ALLOW": "*", "SANDBOX_REGION": "example",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil || cfg.Endpoint != "https://management.example.azuredevcompute.io" || cfg.EgressAllow != nil || !cfg.ProbeRunnerOnline {
		t.Fatalf("unexpected config %+v err=%v", cfg, err)
	}
	if cfg.AutoSuspend != 6*time.Hour+15*time.Minute || cfg.MaxSandboxAge != 6*time.Hour+10*time.Minute {
		t.Fatalf("reapers must outlive the 6h job timeout, got suspend=%s age=%s", cfg.AutoSuspend, cfg.MaxSandboxAge)
	}
	env["AUTO_SUSPEND"] = "15m"
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("expected AUTO_SUSPEND below MAX_JOB_DURATION to be rejected")
	}
	delete(env, "AUTO_SUSPEND")
	env["WARM_POOL"], env["MAX_RUNNERS"] = "5", "2"
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("expected WARM_POOL > MAX_RUNNERS to be rejected")
	}
}

func TestConfigRequiresExplicitSandboxLocation(t *testing.T) {
	env := map[string]string{
		"GITHUB_CONFIG_URL": "https://github.com/o/r", "GITHUB_TOKEN": "t", "SANDBOX_SUBSCRIPTION_ID": "s",
		"SANDBOX_RESOURCE_GROUP": "rg", "SANDBOX_GROUP": "g",
	}
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("expected a missing sandbox endpoint and region to be rejected")
	}
	env["SANDBOX_REGION"] = "example"
	env["SANDBOX_ENDPOINT"] = "https://sandbox.example.test"
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil || cfg.Endpoint != env["SANDBOX_ENDPOINT"] {
		t.Fatalf("explicit endpoint must override the regional endpoint: %+v err=%v", cfg, err)
	}
	delete(env, "SANDBOX_REGION")
	if _, err := loadConfig(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("an explicit endpoint needs no region: %v", err)
	}
	env["GITHUB_CONFIG_URL"] = "https://git.example.test/o/r"
	cfg, err = loadConfig(func(k string) string { return env[k] })
	if err != nil || cfg.ProbeRunnerOnline {
		t.Fatalf("a non-GitHub.com PAT must not be sent to the GitHub.com REST probe: %+v err=%v", cfg, err)
	}
}
