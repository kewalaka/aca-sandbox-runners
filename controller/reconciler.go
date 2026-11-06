package main

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// Reconciler is the backstop for everything the message stream can miss: sandboxes whose job never reported completion
// (controller down, crash, lost message), sandboxes stuck in a dead state, and expired failure snapshots.
type Reconciler struct {
	cfg    Config
	log    *slog.Logger
	sbx    SandboxAPI
	scaler *Scaler
}

func (r *Reconciler) Run(ctx context.Context) {
	r.Sweep(ctx)
	t := time.NewTicker(r.cfg.ReconcileEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Sweep(ctx)
		}
	}
}

func (r *Reconciler) Sweep(ctx context.Context) {
	swept := 0
	sandboxes, err := r.sbx.ListSandboxes(ctx)
	if err != nil {
		r.log.Error("reconcile: list sandboxes", "error", err)
		return
	}
	now := time.Now()
	live := make(map[string]bool, len(sandboxes))
	for _, sb := range sandboxes {
		live[sb.ID] = true
	}
	r.scaler.Prune(live, time.Minute)
	for _, sb := range sandboxes {
		if !r.scaler.owns(sb) || sb.State == "Deleting" {
			continue
		}
		age := now.Sub(sb.CreatedAt)
		var reason string
		switch {
		case age > r.cfg.MaxSandboxAge:
			reason = "max_age"
		case age > r.cfg.OrphanGrace && deadState(sb.State):
			reason = "dead_state_" + strings.ToLower(sb.State)
		default:
			continue
		}
		rn := runnerFromSandbox(sb)
		r.scaler.dispose(ctx, rn, reason)
		swept++
		r.scaler.stage("orphan_swept", "runner", rn.name, "sandbox", sb.ID, "reason", reason, "age_s", int(age.Seconds()), "state", sb.State)
	}

	snaps, err := r.sbx.ListSnapshots(ctx)
	if err != nil {
		r.log.Error("reconcile: list snapshots", "error", err)
		return
	}
	for _, sn := range snaps {
		if sn.Labels[labelPurpose] == purposeFailed && !sn.CreatedAt.IsZero() && now.Sub(sn.CreatedAt) > r.cfg.SnapshotTTL {
			if err := r.sbx.DeleteSnapshot(ctx, sn.ID); err != nil {
				r.log.Error("reconcile: delete snapshot", "snapshot", sn.ID, "error", err)
				continue
			}
			r.scaler.stage("snapshot_expired", "snapshot", sn.ID)
		}
	}
	r.log.Debug("reconcile done", "swept", swept, "sandboxes", len(sandboxes), "snapshots", len(snaps))
}

func deadState(s string) bool {
	switch strings.ToLower(s) {
	case "stopped", "suspended", "failed":
		return true
	}
	return false
}
