package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

const (
	sandboxAPIVersion = "2026-02-01-preview"
	sandboxScope      = "https://dynamicsessions.io/.default"

	labelPurpose  = "purpose"
	purposeRunner = "gh-runner"
	purposeFailed = "gh-runner-failure"
)

type Sandbox struct {
	ID        string            `json:"id"`
	State     string            `json:"state"`
	Labels    map[string]string `json:"labels"`
	CreatedAt time.Time         `json:"createdAt"`

	// Create diagnostics (not part of the API payload).
	PutDur time.Duration `json:"-"`
	Polls  int           `json:"-"`
}

type Snapshot struct {
	ID        string            `json:"id"`
	Labels    map[string]string `json:"labels"`
	CreatedAt time.Time         `json:"createdAtUtc"`
}

type DiskImage struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels"`
	Status struct {
		State     string    `json:"state"`
		CreatedAt time.Time `json:"createdAt"`
	} `json:"status"`
}

type CreateSandboxRequest struct {
	DiskImageID string
	CPU, Memory string
	Env         map[string]string
	Labels      map[string]string
	AutoSuspend time.Duration
	AutoDelete  time.Duration
	EgressAllow []string
}

// SandboxAPI is the subset of the ACA sandbox data plane the controller uses.
type SandboxAPI interface {
	CreateSandbox(ctx context.Context, r CreateSandboxRequest) (*Sandbox, error)
	ListSandboxes(ctx context.Context) ([]Sandbox, error)
	DeleteSandbox(ctx context.Context, id string) error
	Snapshot(ctx context.Context, sandboxID string, labels map[string]string) (*Snapshot, error)
	ListSnapshots(ctx context.Context) ([]Snapshot, error)
	DeleteSnapshot(ctx context.Context, id string) error
	ListDiskImages(ctx context.Context) ([]DiskImage, error)
}

type APIError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("sandbox API %s %s: %d %s", e.Method, e.Path, e.Status, e.Body)
}

func isNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

type sandboxClient struct {
	http *http.Client
	cred azcore.TokenCredential
	base string // endpoint + group resource path
}

func newSandboxClient(cfg Config, cred azcore.TokenCredential) *sandboxClient {
	return &sandboxClient{
		http: &http.Client{Timeout: 5 * time.Minute},
		cred: cred,
		base: fmt.Sprintf("%s/subscriptions/%s/resourceGroups/%s/sandboxGroups/%s",
			strings.TrimRight(cfg.Endpoint, "/"), cfg.SubscriptionID, cfg.ResourceGroup, cfg.Group),
	}
}

// do sends a request, retrying throttling and transient 5xx so that quota/limit behaviour shows up in logs as errors only when persistent.
func (c *sandboxClient) do(ctx context.Context, method, path string, body, out any) error {
	tok, err := c.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{sandboxScope}})
	if err != nil {
		return fmt.Errorf("sandbox token: %w", err)
	}
	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	url := c.base + path + sep + "api-version=" + sandboxAPIVersion

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode < 300 {
			if out != nil && len(data) > 0 {
				return json.Unmarshal(data, out)
			}
			return nil
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if !retryable || attempt >= 3 {
			return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
		}
		wait := time.Duration(1<<attempt) * time.Second
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			wait = time.Duration(s) * time.Second
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *sandboxClient) CreateSandbox(ctx context.Context, r CreateSandboxRequest) (*Sandbox, error) {
	body := map[string]any{
		"sourcesRef": map[string]any{"diskImage": map[string]any{"id": r.DiskImageID}},
		"resources":  map[string]any{"cpu": r.CPU, "memory": r.Memory},
		"labels":     r.Labels,
		"lifecycle": map[string]any{
			"autoSuspendPolicy": map[string]any{"enabled": true, "interval": int(r.AutoSuspend.Seconds()), "mode": "Memory"},
			"autoDeletePolicy":  map[string]any{"enabled": true, "deleteIntervalInSeconds": int(r.AutoDelete.Seconds())},
		},
	}
	if len(r.Env) > 0 {
		body["environment"] = r.Env
	}
	if len(r.EgressAllow) > 0 {
		rules := make([]map[string]string, 0, len(r.EgressAllow))
		for _, p := range r.EgressAllow {
			rules = append(rules, map[string]string{"pattern": p, "action": "Allow"})
		}
		body["egressPolicy"] = map[string]any{"defaultAction": "Deny", "hostRules": rules}
	}

	var sbx Sandbox
	t0 := time.Now()
	if err := c.do(ctx, http.MethodPut, "/sandboxes", body, &sbx); err != nil {
		return nil, err
	}
	putDur, polls := time.Since(t0), 0
	deadline := time.Now().Add(5 * time.Minute)
	for sbx.State != "Running" {
		if sbx.State == "Failed" || sbx.State == "Deleting" {
			return &sbx, fmt.Errorf("sandbox %s entered state %s", sbx.ID, sbx.State)
		}
		if time.Now().After(deadline) {
			return &sbx, fmt.Errorf("sandbox %s not Running after 5m (state %s)", sbx.ID, sbx.State)
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return &sbx, ctx.Err()
		}
		polls++
		if err := c.do(ctx, http.MethodGet, "/sandboxes/"+sbx.ID, nil, &sbx); err != nil {
			return &sbx, err
		}
	}
	sbx.PutDur, sbx.Polls = putDur, polls
	return &sbx, nil
}

func (c *sandboxClient) ListSandboxes(ctx context.Context) (out []Sandbox, err error) {
	err = c.do(ctx, http.MethodGet, "/sandboxes", nil, &out)
	return
}

func (c *sandboxClient) DeleteSandbox(ctx context.Context, id string) error {
	if err := c.do(ctx, http.MethodDelete, "/sandboxes/"+id, nil, nil); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func (c *sandboxClient) Snapshot(ctx context.Context, id string, labels map[string]string) (*Snapshot, error) {
	var s Snapshot
	err := c.do(ctx, http.MethodPost, "/sandboxes/"+id+"/snapshot", map[string]any{"labels": labels}, &s)
	return &s, err
}

func (c *sandboxClient) ListSnapshots(ctx context.Context) (out []Snapshot, err error) {
	err = c.do(ctx, http.MethodGet, "/snapshots", nil, &out)
	return
}

func (c *sandboxClient) DeleteSnapshot(ctx context.Context, id string) error {
	if err := c.do(ctx, http.MethodDelete, "/snapshots/"+id, nil, nil); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func (c *sandboxClient) ListDiskImages(ctx context.Context) (out []DiskImage, err error) {
	err = c.do(ctx, http.MethodGet, "/diskimages", nil, &out)
	return
}
