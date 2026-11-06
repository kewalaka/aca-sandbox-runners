package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// ScaleSetAPI is the part of *scaleset.Client the scaler needs.
type ScaleSetAPI interface {
	GenerateJitRunnerConfig(ctx context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

type runner struct {
	name      string
	runnerID  int
	sandboxID string
	createdAt time.Time
	busy      bool
}

// Scaler turns scale-set demand into sandboxes: one sandbox == one JIT runner == one job.
// State is in memory but fully reconstructible from sandbox labels (see Adopt).
type Scaler struct {
	cfg        Config
	log        *slog.Logger
	sbx        SandboxAPI
	ss         ScaleSetAPI
	scaleSetID int
	image      func(context.Context) (string, error)
	probe      func(ctx context.Context, name string, runnerID int, since time.Time)

	mu      sync.Mutex
	runners map[string]*runner
	wg      sync.WaitGroup
}

var _ listener.Scaler = (*Scaler)(nil)

func newScaler(cfg Config, log *slog.Logger, sbx SandboxAPI, ss ScaleSetAPI, scaleSetID int, image func(context.Context) (string, error)) *Scaler {
	return &Scaler{cfg: cfg, log: log, sbx: sbx, ss: ss, scaleSetID: scaleSetID, image: image, runners: map[string]*runner{}}
}

// stage logs one pipeline stage; analysis scripts decompose start latency from these records.
func (s *Scaler) stage(name string, attrs ...any) {
	s.log.Info(name, append([]any{"stage", name}, attrs...)...)
}

func (s *Scaler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runners)
}

func (s *Scaler) track(r *runner) {
	s.mu.Lock()
	s.runners[r.name] = r
	s.mu.Unlock()
}

func (s *Scaler) get(name string) (runner, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runners[name]; ok {
		return *r, true
	}
	return runner{}, false
}

func (s *Scaler) forget(name string) {
	s.mu.Lock()
	delete(s.runners, name)
	s.mu.Unlock()
}

// Prune drops tracked runners whose sandbox has disappeared (deleted by a previous replica, auto-delete, ...);
// otherwise they would count against the desired runner count forever.
func (s *Scaler) Prune(live map[string]bool, grace time.Duration) {
	s.mu.Lock()
	var gone []string
	for name, r := range s.runners {
		if r.sandboxID != "" && !live[r.sandboxID] && time.Since(r.createdAt) > grace {
			gone = append(gone, name)
			delete(s.runners, name)
		}
	}
	s.mu.Unlock()
	for _, name := range gone {
		s.stage("runner_pruned", "runner", name)
	}
}

func (s *Scaler) sandboxLabels(name string, runnerID int) map[string]string {
	return map[string]string{
		labelPurpose: purposeRunner,
		"controller": s.cfg.ScaleSetName,
		"scaleset":   strconv.Itoa(s.scaleSetID),
		"runner":     name,
		"runnerid":   strconv.Itoa(runnerID),
	}
}

// Adopt rebuilds state from sandboxes this scale set created before a restart, so their JobCompleted messages dispose them.
func (s *Scaler) Adopt(ctx context.Context) error {
	list, err := s.sbx.ListSandboxes(ctx)
	if err != nil {
		return err
	}
	for _, sb := range list {
		if !s.owns(sb) || sb.State == "Deleting" {
			continue
		}
		r := runnerFromSandbox(sb)
		if _, known := s.get(r.name); known {
			continue
		}
		s.track(&r)
		s.stage("sandbox_adopted", "runner", r.name, "sandbox", sb.ID, "state", sb.State)
	}
	return nil
}

func (s *Scaler) owns(sb Sandbox) bool {
	return sb.Labels[labelPurpose] == purposeRunner && sb.Labels["scaleset"] == strconv.Itoa(s.scaleSetID)
}

// HandleDesiredRunnerCount keeps WarmPool idle runners plus one per assigned job, capped at MaxRunners.
func (s *Scaler) HandleDesiredRunnerCount(ctx context.Context, assigned int) (int, error) {
	target := min(s.cfg.MaxRunners, s.cfg.WarmPool+assigned)
	need := target - s.count()
	if need <= 0 {
		return s.count(), nil
	}
	s.stage("scale_up", "assigned", assigned, "target", target, "creating", need)

	sem := make(chan struct{}, max(1, s.cfg.CreateConcurrent))
	var wg sync.WaitGroup
	for range need {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.startRunner(ctx); err != nil {
				// Not fatal: the next message or the idle poll re-evaluates demand and retries.
				s.log.Error("runner create failed", "stage", "create_failed", "error", err)
			}
		}()
	}
	wg.Wait()
	return s.count(), ctx.Err()
}

func (s *Scaler) startRunner(ctx context.Context) error {
	t0 := time.Now()
	name := fmt.Sprintf("sbx-%d-%s", s.scaleSetID, randHex(4))
	// Reserve the slot so concurrent evaluations don't over-create.
	s.track(&runner{name: name, createdAt: t0})

	jit, err := s.ss.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, s.scaleSetID)
	if err != nil {
		s.forget(name)
		return fmt.Errorf("mint JIT config: %w", err)
	}
	s.stage("jit_minted", "runner", name, "runner_id", jit.Runner.ID, "dur_ms", ms(time.Since(t0)))

	fail := func(err error) error {
		s.forget(name)
		if rmErr := s.ss.RemoveRunner(context.WithoutCancel(ctx), int64(jit.Runner.ID)); rmErr != nil {
			s.log.Warn("remove runner after failed create", "runner", name, "error", rmErr)
		}
		return err
	}

	imageID, err := s.image(ctx)
	if err != nil {
		return fail(fmt.Errorf("resolve disk image: %w", err))
	}
	t1 := time.Now()
	sbx, err := s.sbx.CreateSandbox(ctx, CreateSandboxRequest{
		DiskImageID: imageID, CPU: s.cfg.CPU, Memory: s.cfg.Memory,
		Env:         map[string]string{"JITCONFIG": jit.EncodedJITConfig},
		Labels:      s.sandboxLabels(name, jit.Runner.ID),
		AutoSuspend: s.cfg.AutoSuspend, AutoDelete: s.cfg.AutoDelete, EgressAllow: s.cfg.EgressAllow,
	})
	if err != nil {
		if sbx != nil && sbx.ID != "" {
			_ = s.sbx.DeleteSandbox(context.WithoutCancel(ctx), sbx.ID)
		}
		return fail(fmt.Errorf("create sandbox: %w", err))
	}
	s.track(&runner{name: name, runnerID: jit.Runner.ID, sandboxID: sbx.ID, createdAt: time.Now()})
	s.stage("sandbox_created", "runner", name, "sandbox", sbx.ID, "dur_ms", ms(time.Since(t1)), "put_ms", ms(sbx.PutDur), "polls", sbx.Polls, "since_demand_ms", ms(time.Since(t0)))

	if s.probe != nil {
		go s.probe(context.WithoutCancel(ctx), name, jit.Runner.ID, time.Now())
	}
	return nil
}

func (s *Scaler) HandleJobStarted(ctx context.Context, m *scaleset.JobStarted) error {
	s.mu.Lock()
	if r, ok := s.runners[m.RunnerName]; ok {
		r.busy = true
	}
	s.mu.Unlock()
	s.stage("job_started", "runner", m.RunnerName, "job_id", m.JobID, "run_id", m.WorkflowRunID, "job", m.JobDisplayName)
	return nil
}

func (s *Scaler) HandleJobCompleted(ctx context.Context, m *scaleset.JobCompleted) error {
	s.stage("job_completed", "runner", m.RunnerName, "job_id", m.JobID, "run_id", m.WorkflowRunID, "result", m.Result)
	r, ok := s.get(m.RunnerName)
	if !ok {
		// A previous replica (rolling update) may have created it after our startup adoption.
		if err := s.Adopt(ctx); err != nil {
			s.log.Warn("re-adopt failed", "error", err)
		}
		r, ok = s.get(m.RunnerName)
	}
	if !ok {
		s.log.Warn("completed job for untracked runner; reconciler will sweep any leftover sandbox", "runner", m.RunnerName)
		return nil
	}
	if s.cfg.KeepOnFailure && slices.ContainsFunc(s.cfg.SnapshotResults, func(x string) bool { return strings.EqualFold(x, m.Result) }) {
		s.snapshot(ctx, r, m)
	}
	s.dispose(ctx, r, "job_completed")
	return nil
}

// snapshot preserves a failed sandbox for forensics; it must never block disposal.
func (s *Scaler) snapshot(ctx context.Context, r runner, m *scaleset.JobCompleted) {
	t := time.Now()
	snap, err := s.sbx.Snapshot(ctx, r.sandboxID, map[string]string{
		labelPurpose: purposeFailed, "runner": r.name, "job": m.JobID, "run": strconv.FormatInt(m.WorkflowRunID, 10),
		"repo": m.OwnerName + "/" + m.RepositoryName, "result": m.Result,
	})
	if err != nil {
		s.log.Error("snapshot failed", "stage", "snapshot_failed", "runner", r.name, "error", err)
		return
	}
	s.stage("snapshot_created", "runner", r.name, "snapshot", snap.ID, "job_id", m.JobID, "dur_ms", ms(time.Since(t)))
}

// dispose frees the runner's slot immediately and deletes the sandbox and runner registration in the background
// (a sandbox delete takes 5-8 s and the listener handles messages serially). A delete that keeps failing leaves a
// labelled orphan for the reconciler.
func (s *Scaler) dispose(ctx context.Context, r runner, reason string) {
	ctx = context.WithoutCancel(ctx)
	s.forget(r.name)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.Now()
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if r.sandboxID == "" {
				break
			}
			if err = s.sbx.DeleteSandbox(ctx, r.sandboxID); err == nil {
				break
			}
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		}
		if err != nil {
			s.log.Error("sandbox delete failed; reconciler will retry", "stage", "delete_failed", "runner", r.name, "sandbox", r.sandboxID, "error", err)
			return
		}
		if r.runnerID != 0 {
			if err := s.ss.RemoveRunner(ctx, int64(r.runnerID)); err != nil {
				s.log.Debug("runner already removed", "runner", r.name, "error", err)
			}
		}
		s.stage("sandbox_deleted", "runner", r.name, "sandbox", r.sandboxID, "reason", reason, "dur_ms", ms(time.Since(t)))
	}()
}

// Wait blocks until background deletions finish.
func (s *Scaler) Wait() { s.wg.Wait() }

func runnerFromSandbox(sb Sandbox) runner {
	id, _ := strconv.Atoi(sb.Labels["runnerid"])
	return runner{name: sb.Labels["runner"], runnerID: id, sandboxID: sb.ID, createdAt: sb.CreatedAt}
}

// Shutdown deletes idle runners only; busy ones keep running and are re-adopted by the next start.
func (s *Scaler) Shutdown(ctx context.Context) {
	s.mu.Lock()
	var idle []runner
	for _, r := range s.runners {
		if !r.busy && r.sandboxID != "" {
			idle = append(idle, *r)
		}
	}
	s.mu.Unlock()
	for _, r := range idle {
		s.dispose(ctx, r, "shutdown")
	}
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
