package controller

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v88/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/config"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/githubapp"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/vm"
)

func TestMatchLabels(t *testing.T) {
	runner := []string{"self-hosted", "linux", "x64", "fcghact"}
	tests := []struct {
		name    string
		job     []string
		runner  []string
		require []string
		want    bool
	}{
		{name: "subset with required label", job: []string{"self-hosted", "fcghact"}, runner: runner, require: []string{"fcghact"}, want: true},
		{name: "case insensitive", job: []string{"SELF-HOSTED", "FCGHACT"}, runner: runner, require: []string{"fcghact"}, want: true},
		{name: "extra job label", job: []string{"self-hosted", "fcghact", "gpu"}, runner: runner, require: []string{"fcghact"}, want: false},
		{name: "missing required label", job: []string{"self-hosted", "linux"}, runner: runner, require: []string{"fcghact"}, want: false},
		{name: "no required labels", job: []string{"linux"}, runner: runner, want: true},
		{name: "empty job labels without requirements", job: nil, runner: runner, want: true},
		{name: "empty job labels with requirements", job: nil, runner: runner, require: []string{"fcghact"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MatchLabels(tt.job, tt.runner, tt.require))
		})
	}
}

func TestJITLabels(t *testing.T) {
	assert.Equal(t, []string{"fcghact", "gpu"}, JITLabels([]string{"self-hosted", "Linux", "X64", "fcghact", "gpu"}))
	assert.Equal(t, []string{"self-hosted"}, JITLabels([]string{"self-hosted"}))
}

func testConfig() *config.Config {
	cfg := config.Default()
	cfg.GitHub.AppID = 1
	cfg.GitHub.PrivateKeyPath = "/tmp/key.pem"
	cfg.GitHub.WebhookSecret = "secret"
	cfg.Runner.MonitorInterval = config.Duration(50 * time.Millisecond)
	cfg.Runner.MaxConcurrentJobs = 2
	cfg.Runner.MaxJobsPerRepo = 2
	return cfg
}

func queuedEvent(id int64, labels []string, repo string) *github.WorkflowJobEvent {
	owner, name, _ := splitRepo(repo)
	return &github.WorkflowJobEvent{
		Action: github.Ptr("queued"),
		WorkflowJob: &github.WorkflowJob{
			ID:     github.Ptr(id),
			Status: github.Ptr("queued"),
			Labels: labels,
		},
		Repo: &github.Repository{
			Name:     github.Ptr(name),
			FullName: github.Ptr(repo),
			Owner:    &github.User{Login: github.Ptr(owner)},
		},
		Installation: &github.Installation{ID: github.Ptr(int64(7))},
	}
}

func splitRepo(repo string) (string, string, bool) {
	for i := 0; i < len(repo); i++ {
		if repo[i] == '/' {
			return repo[:i], repo[i+1:], true
		}
	}
	return "", repo, false
}

type fakeAPI struct {
	mu         sync.Mutex
	requests   []githubapp.JITRequest
	runnerName string
	status     githubapp.JobStatus
	jitErr     error
}

func (f *fakeAPI) GenerateJITConfig(_ context.Context, _ githubapp.Scope, req githubapp.JITRequest) (*githubapp.JITConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jitErr != nil {
		return nil, f.jitErr
	}
	f.requests = append(f.requests, req)
	f.runnerName = req.Name
	return &githubapp.JITConfig{Encoded: "encoded-jit", RunnerName: req.Name, RunnerID: 5}, nil
}

func (f *fakeAPI) GetJobStatus(_ context.Context, _ githubapp.Scope, id int64) (*githubapp.JobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := f.status
	if status.Status == "" {
		status.Status = "queued"
	}
	if status.RunnerName == "" && f.runnerName != "" && status.Status != "queued" {
		status.RunnerName = f.runnerName
	}
	status.ID = id
	return &status, nil
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeAPI) lastRequest() githubapp.JITRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return githubapp.JITRequest{}
	}
	return f.requests[len(f.requests)-1]
}

type fakeBackend struct {
	mu      sync.Mutex
	specs   []vm.Spec
	handles []*fakeHandle
}

func (b *fakeBackend) Start(_ context.Context, spec vm.Spec) (vm.Handle, error) {
	h := &fakeHandle{id: spec.ID, runner: spec.RunnerName, done: make(chan struct{})}
	b.mu.Lock()
	b.specs = append(b.specs, spec)
	b.handles = append(b.handles, h)
	b.mu.Unlock()
	return h, nil
}

func (b *fakeBackend) Reap(context.Context) error { return nil }

func (b *fakeBackend) Shutdown(context.Context) error {
	b.mu.Lock()
	handles := append([]*fakeHandle(nil), b.handles...)
	b.mu.Unlock()
	for _, h := range handles {
		h.stop()
	}
	return nil
}

func (b *fakeBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.specs)
}

func (b *fakeBackend) handleAt(i int) *fakeHandle {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.handles[i]
}

type fakeHandle struct {
	id     string
	runner string
	done   chan struct{}
	once   sync.Once
	stops  int
	mu     sync.Mutex
}

func (h *fakeHandle) ID() string           { return h.id }
func (h *fakeHandle) IP() string           { return "172.31.0.2" }
func (h *fakeHandle) RunnerName() string   { return h.runner }
func (h *fakeHandle) StartedAt() time.Time { return time.Now() }

func (h *fakeHandle) Wait() error {
	<-h.done
	return nil
}

func (h *fakeHandle) Stop(context.Context) error {
	h.mu.Lock()
	h.stops++
	h.mu.Unlock()
	h.stop()
	return nil
}

func (h *fakeHandle) stop() { h.once.Do(func() { close(h.done) }) }

func (h *fakeHandle) stopped() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

func TestControllerDispatchesAndStopsOnCompletion(t *testing.T) {
	cfg := testConfig()
	api := &fakeAPI{status: githubapp.JobStatus{Status: "in_progress"}}
	backend := &fakeBackend{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctrl := New(cfg, api, backend, log)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctrl.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = ctrl.Shutdown(context.Background())
		<-done
	})

	event := queuedEvent(101, []string{"self-hosted", "fcghact"}, "owner/repo")
	require.NoError(t, ctrl.HandleWorkflowJob(ctx, event, "delivery-1"))

	require.Eventually(t, func() bool { return api.count() == 1 && backend.count() == 1 }, 3*time.Second, 10*time.Millisecond)

	req := api.lastRequest()
	assert.Equal(t, []string{"fcghact"}, req.Labels)
	assert.Equal(t, int64(1), req.RunnerGroupID)
	assert.Contains(t, req.Name, "-j101-")

	handle := backend.handleAt(0)
	completed := &github.WorkflowJobEvent{
		Action: github.Ptr("completed"),
		WorkflowJob: &github.WorkflowJob{
			ID:         github.Ptr(int64(101)),
			RunnerName: github.Ptr(req.Name),
			Conclusion: github.Ptr("success"),
		},
	}
	require.NoError(t, ctrl.HandleWorkflowJob(ctx, completed, "delivery-2"))
	require.Eventually(t, handle.stopped, 3*time.Second, 10*time.Millisecond)
}

func TestControllerDeduplicatesQueuedDeliveries(t *testing.T) {
	cfg := testConfig()
	api := &fakeAPI{status: githubapp.JobStatus{Status: "in_progress"}}
	backend := &fakeBackend{}
	ctrl := New(cfg, api, backend, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctrl.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = ctrl.Shutdown(context.Background())
		<-done
	})

	event := queuedEvent(202, []string{"self-hosted", "fcghact"}, "owner/repo")
	require.NoError(t, ctrl.HandleWorkflowJob(ctx, event, "a"))
	require.Eventually(t, func() bool { return backend.count() == 1 }, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, ctrl.HandleWorkflowJob(ctx, event, "b"))
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 1, backend.count())
	assert.Equal(t, 1, api.count())
}

func TestControllerIgnoresNonMatchingJobs(t *testing.T) {
	cfg := testConfig()
	cfg.GitHub.Repositories = []string{"allowed/repo"}
	api := &fakeAPI{}
	backend := &fakeBackend{}
	ctrl := New(cfg, api, backend, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ctrl.Run(ctx)

	// Missing the required fcghact label.
	require.NoError(t, ctrl.HandleWorkflowJob(ctx, queuedEvent(1, []string{"self-hosted", "linux"}, "allowed/repo"), "a"))
	// Repository outside the allow list.
	require.NoError(t, ctrl.HandleWorkflowJob(ctx, queuedEvent(2, []string{"self-hosted", "fcghact"}, "other/repo"), "b"))
	// Already assigned to a runner.
	assigned := queuedEvent(3, []string{"self-hosted", "fcghact"}, "allowed/repo")
	assigned.WorkflowJob.RunnerID = github.Ptr(int64(77))
	require.NoError(t, ctrl.HandleWorkflowJob(ctx, assigned, "c"))

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, api.count())
	assert.Equal(t, 0, backend.count())
}

func TestControllerEnforcesPerRepoLimit(t *testing.T) {
	cfg := testConfig()
	cfg.Runner.MaxConcurrentJobs = 2
	cfg.Runner.MaxJobsPerRepo = 1
	api := &fakeAPI{status: githubapp.JobStatus{Status: "in_progress"}}
	backend := &fakeBackend{}
	ctrl := New(cfg, api, backend, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctrl.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = ctrl.Shutdown(context.Background())
		<-done
	})

	require.NoError(t, ctrl.HandleWorkflowJob(ctx, queuedEvent(1, []string{"self-hosted", "fcghact"}, "owner/repo"), "a"))
	require.NoError(t, ctrl.HandleWorkflowJob(ctx, queuedEvent(2, []string{"self-hosted", "fcghact"}, "owner/repo"), "b"))
	require.Eventually(t, func() bool { return backend.count() == 1 }, 3*time.Second, 10*time.Millisecond)

	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 1, backend.count(), "second job must wait for the repository slot")

	// Freeing the slot must let the queued job start.
	require.NoError(t, ctrl.HandleWorkflowJob(ctx, &github.WorkflowJobEvent{
		Action:      github.Ptr("completed"),
		WorkflowJob: &github.WorkflowJob{ID: github.Ptr(int64(1)), RunnerName: github.Ptr(api.lastRequest().Name)},
	}, "c"))
	require.Eventually(t, func() bool { return backend.count() == 2 }, 3*time.Second, 10*time.Millisecond)
}
