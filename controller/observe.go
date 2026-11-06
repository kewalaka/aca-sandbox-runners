package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// observedClient wraps the message session to timestamp when each demand/job message reaches the controller.
// The listener hides JobAvailable/JobAssigned from the Scaler, so this is the only place "demand seen" can be logged.
type observedClient struct {
	listener.Client
	log *slog.Logger
}

func (c observedClient) GetMessage(ctx context.Context, last, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	msg, err := c.Client.GetMessage(ctx, last, maxCapacity)
	if err != nil || msg == nil {
		return msg, err
	}
	now := time.Now()
	st := msg.Statistics
	c.log.Info("message", "stage", "message", "id", msg.MessageID,
		"available", len(msg.JobAvailableMessages), "assigned", len(msg.JobAssignedMessages),
		"started", len(msg.JobStartedMessages), "completed", len(msg.JobCompletedMessages),
		"total_assigned", st.TotalAssignedJobs, "total_running", st.TotalRunningJobs, "idle_runners", st.TotalIdleRunners)
	for _, j := range msg.JobAvailableMessages {
		c.log.Info("demand_seen", append(jobAttrs("demand_seen", &j.JobMessageBase), "queue_to_seen_ms", since(now, j.QueueTime))...)
	}
	for _, j := range msg.JobAssignedMessages {
		c.log.Info("job_assigned", jobAttrs("job_assigned", &j.JobMessageBase)...)
	}
	for _, j := range msg.JobStartedMessages {
		c.log.Info("job_started_msg", jobAttrs("job_started_msg", &j.JobMessageBase)...)
	}
	for _, j := range msg.JobCompletedMessages {
		c.log.Info("job_completed_msg", jobAttrs("job_completed_msg", &j.JobMessageBase)...)
	}
	return msg, nil
}

// imageResolver returns the pinned disk image or the newest Ready one with a matching name label, cached briefly so
// freshly built images roll out without a restart.
func imageResolver(cfg Config, sbx SandboxAPI) func(context.Context) (string, error) {
	var (
		mu      sync.Mutex
		id      string
		fetched time.Time
	)
	return func(ctx context.Context) (string, error) {
		if cfg.DiskImageID != "" {
			return cfg.DiskImageID, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if id != "" && time.Since(fetched) < 5*time.Minute {
			return id, nil
		}
		imgs, err := sbx.ListDiskImages(ctx)
		if err != nil {
			return "", err
		}
		var ready []DiskImage
		for _, i := range imgs {
			if i.Status.State == "Ready" && strings.HasPrefix(i.Labels["name"], cfg.DiskImagePrefix) {
				ready = append(ready, i)
			}
		}
		if len(ready) == 0 {
			return "", errNoImage
		}
		sort.Slice(ready, func(a, b int) bool { return ready[a].Status.CreatedAt.After(ready[b].Status.CreatedAt) })
		id, fetched = ready[0].ID, time.Now()
		return id, nil
	}
}

var errNoImage = &APIError{Method: "GET", Path: "/diskimages", Status: http.StatusNotFound, Body: "no Ready disk image with the configured name prefix; run the image workflow"}

// onlineProbe timestamps when GitHub reports the runner online (REST, PAT only). It is a measurement aid, not control flow.
func onlineProbe(cfg Config, log *slog.Logger, stillTracked func(string) bool) func(context.Context, string, int, time.Time) {
	owner, repo, ok := cfg.ownerRepo()
	if !cfg.ProbeRunnerOnline || !ok {
		return nil
	}
	client := &http.Client{Timeout: 15 * time.Second}
	url := func(id int) string {
		return "https://api.github.com/repos/" + owner + "/" + repo + "/actions/runners/" + strconv.Itoa(id)
	}
	return func(ctx context.Context, name string, runnerID int, since time.Time) {
		for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline) && stillTracked(name); {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url(runnerID), nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			req.Header.Set("Accept", "application/vnd.github+json")
			if resp, err := client.Do(req); err == nil {
				var body struct {
					Status string `json:"status"`
				}
				ok := resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil && body.Status == "online"
				resp.Body.Close()
				if ok {
					log.Info("runner_online", "stage", "runner_online", "runner", name, "since_create_ms", time.Since(since).Milliseconds())
					return
				}
			}
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return
			}
		}
	}
}

// jobAttrs carries GitHub's own timestamps (zero values are omitted) so GitHub-side and controller-side latency can be joined by job id.
func jobAttrs(stage string, m *scaleset.JobMessageBase) []any {
	a := []any{"stage", stage, "job_id", m.JobID, "run_id", m.WorkflowRunID, "job", m.JobDisplayName}
	for k, t := range map[string]time.Time{
		"queue_time": m.QueueTime, "scaleset_assign_time": m.ScaleSetAssignTime, "runner_assign_time": m.RunnerAssignTime, "finish_time": m.FinishTime,
	} {
		if !t.IsZero() {
			a = append(a, k, t)
		}
	}
	return a
}

func since(now, t time.Time) int64 {
	if t.IsZero() {
		return -1
	}
	return now.Sub(t).Milliseconds()
}
