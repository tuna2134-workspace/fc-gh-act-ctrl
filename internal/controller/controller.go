// Package controller maps GitHub workflow_job events to disposable
// Firecracker microVMs.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v88/github"
	"github.com/google/uuid"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/config"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/githubapp"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/metrics"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/vm"
)

// Job is a workflow job accepted for execution.
type Job struct {
	ID             int64
	RunID          int64
	Repo           githubapp.Repo
	Org            string
	InstallationID int64
	Labels         []string
	WorkflowName   string
	HTMLURL        string
	EnqueuedAt     time.Time
}

// Controller owns the job lifecycle.
type Controller struct {
	cfg *config.Config
	api githubapp.API
	vms vm.Backend
	log *slog.Logger

	sched    *scheduler
	claims   *claimSet
	sessions *sessionRegistry
}

// New builds a controller. The caller must run Run and call Shutdown on exit.
func New(cfg *config.Config, api githubapp.API, backend vm.Backend, log *slog.Logger) *Controller {
	if log == nil {
		log = slog.Default()
	}
	c := &Controller{
		cfg:      cfg,
		api:      api,
		vms:      backend,
		log:      log,
		claims:   newClaimSet(3 * time.Hour),
		sessions: newSessionRegistry(),
	}
	c.sched = newScheduler(cfg.Runner, log, c.startJob)
	return c
}

// Run drives the scheduler and the reconciliation loop until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.sched.run(ctx)
	}()
	go func() {
		defer wg.Done()
		c.monitor(ctx)
	}()
	wg.Wait()
}

// Shutdown stops accepting jobs and terminates every running VM.
func (c *Controller) Shutdown(ctx context.Context) error {
	c.sched.stop()
	return c.vms.Shutdown(ctx)
}

// HandleWorkflowJob implements webhook.Sink.
func (c *Controller) HandleWorkflowJob(ctx context.Context, event *github.WorkflowJobEvent, deliveryID string) error {
	action := ""
	if event.Action != nil {
		action = *event.Action
	}
	log := c.log.With("delivery", deliveryID, "action", action)

	if event.WorkflowJob == nil {
		metrics.Webhooks.WithLabelValues(action, "ignored").Inc()
		return nil
	}
	job := event.WorkflowJob
	log = log.With("job_id", job.GetID())

	repo, org, installationID := eventTarget(event)
	log = log.With("repository", repo.String())

	switch action {
	case "queued":
		result, err := c.handleQueued(job, repo, org, installationID, log)
		metrics.Webhooks.WithLabelValues(action, result).Inc()
		return err
	case "in_progress":
		c.handleStarted(job, log)
		metrics.Webhooks.WithLabelValues(action, "observed").Inc()
	case "completed":
		c.handleCompleted(job, log)
		metrics.Webhooks.WithLabelValues(action, "observed").Inc()
	default:
		metrics.Webhooks.WithLabelValues(action, "ignored").Inc()
	}
	return nil
}

func eventTarget(event *github.WorkflowJobEvent) (githubapp.Repo, string, int64) {
	var repo githubapp.Repo
	if event.Repo != nil {
		repo.Name = event.Repo.GetName()
		if owner := event.Repo.GetOwner(); owner != nil {
			repo.Owner = owner.GetLogin()
		}
	}
	var org string
	if event.Org != nil {
		org = event.Org.GetLogin()
	}
	var installationID int64
	if event.Installation != nil {
		installationID = event.Installation.GetID()
	}
	return repo, org, installationID
}

func (c *Controller) handleQueued(job *github.WorkflowJob, repo githubapp.Repo, org string, installationID int64, log *slog.Logger) (string, error) {
	if !c.allowed(repo, org) {
		log.Debug("ignoring job for a repository outside the allow list")
		return "ignored", nil
	}
	if status := job.GetStatus(); status != "" && status != "queued" {
		log.Debug("ignoring job with non-queued status", "status", status)
		return "ignored", nil
	}
	if job.GetRunnerID() != 0 {
		log.Debug("ignoring job that already has a runner", "runner", job.GetRunnerName())
		return "ignored", nil
	}
	if !MatchLabels(job.Labels, c.cfg.Runner.Labels, c.cfg.Runner.RequireLabels) {
		log.Debug("ignoring job with non-matching labels", "labels", job.Labels)
		return "ignored", nil
	}

	j := &Job{
		ID:             job.GetID(),
		RunID:          job.GetRunID(),
		Repo:           repo,
		Org:            org,
		InstallationID: installationID,
		Labels:         append([]string(nil), job.Labels...),
		WorkflowName:   job.GetWorkflowName(),
		HTMLURL:        job.GetHTMLURL(),
		EnqueuedAt:     time.Now(),
	}
	if !c.claims.acquire(j.ID) {
		log.Debug("ignoring duplicate delivery for an already claimed job")
		return "duplicate", nil
	}
	if err := c.sched.Enqueue(j); err != nil {
		c.claims.release(j.ID)
		log.Warn("rejecting job: queue is full", "error", err)
		return "rejected", err
	}
	log.Info("queued job",
		"labels", j.Labels,
		"workflow", j.WorkflowName,
		"url", j.HTMLURL,
	)
	return "accepted", nil
}

func (c *Controller) handleStarted(job *github.WorkflowJob, log *slog.Logger) {
	sess := c.findSession(job)
	if sess == nil {
		return
	}
	log.Info("job picked up by runner",
		"runner", sess.runnerName,
		"vm", sess.handleID(),
	)
}

func (c *Controller) handleCompleted(job *github.WorkflowJob, log *slog.Logger) {
	sess := c.findSession(job)
	if sess == nil {
		return
	}
	log.Info("job completed, stopping microvm",
		"runner", sess.runnerName,
		"conclusion", job.GetConclusion(),
		"vm", sess.handleID(),
	)
	sess.requestStop()
}

func (c *Controller) findSession(job *github.WorkflowJob) *session {
	if name := job.GetRunnerName(); name != "" {
		if sess, ok := c.sessions.byRunnerName(name); ok {
			return sess
		}
	}
	if sess, ok := c.sessions.byJobID(job.GetID()); ok {
		return sess
	}
	return nil
}

// startJob runs on the scheduler goroutine pool and owns one job for its whole
// lifetime.
func (c *Controller) startJob(ctx context.Context, job *Job) {
	defer func() {
		c.claims.release(job.ID)
		c.sched.finish(job.ID)
	}()

	scope := c.scopeFor(job)
	const maxAttempts = 2

	var (
		handle     vm.Handle
		sess       *session
		runnerName string
		startedAt  time.Time
	)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		runnerName = c.runnerName(job.ID)
		startedAt = time.Now()

		c.log.Info("requesting just-in-time runner", "job_id", job.ID, "runner", runnerName, "attempt", attempt)
		jit, err := c.api.GenerateJITConfig(ctx, scope, githubapp.JITRequest{
			Name:          runnerName,
			Labels:        JITLabels(c.cfg.Runner.Labels),
			RunnerGroupID: c.cfg.Runner.RunnerGroupID,
			WorkFolder:    c.cfg.Runner.WorkFolder,
		})
		if err != nil {
			// GenerateJITConfig already retries transient failures.
			metrics.JobFailures.WithLabelValues("jit_config").Inc()
			metrics.JobsFinished.WithLabelValues("start_failed").Inc()
			c.log.Error("failed to create just-in-time runner", "job_id", job.ID, "error", err)
			return
		}

		sess = newSession(job, runnerName, c.log)
		c.sessions.add(sess)
		handle, err = c.vms.Start(ctx, vm.Spec{
			ID:         uuid.NewString(),
			JobID:      job.ID,
			Repository: job.Repo.String(),
			RunnerName: runnerName,
			JITConfig:  jit.Encoded,
			WorkFolder: c.cfg.Runner.WorkFolder,
		})
		if err != nil {
			c.sessions.remove(sess)
			sess = nil
			handle = nil
			metrics.JobFailures.WithLabelValues("vm_start").Inc()
			c.log.Error("failed to start microvm", "job_id", job.ID, "runner", runnerName, "attempt", attempt, "error", err)
			continue
		}
		break
	}
	if sess == nil || handle == nil {
		metrics.JobsFinished.WithLabelValues("start_failed").Inc()
		return
	}
	defer c.sessions.remove(sess)

	sess.attach(handle)
	metrics.VMsActive.Inc()
	defer metrics.VMsActive.Dec()
	metrics.VMStartSeconds.Observe(time.Since(startedAt).Seconds())
	metrics.JobsDispatched.WithLabelValues(job.Repo.String()).Inc()

	c.log.Info("microvm started for job",
		"job_id", job.ID,
		"runner", runnerName,
		"vm", handle.ID(),
		"ip", handle.IP(),
	)

	waitErr := handle.Wait()
	lifetime := time.Since(handle.StartedAt())

	result := "completed"
	switch {
	case sess.stopWasRequested():
		result = "stopped"
	case waitErr != nil:
		result = "vm_exit_error"
	}
	metrics.JobSeconds.Observe(lifetime.Seconds())
	metrics.JobsFinished.WithLabelValues(result).Inc()
	c.log.Info("microvm finished",
		"job_id", job.ID,
		"runner", runnerName,
		"vm", handle.ID(),
		"result", result,
		"lifetime", lifetime.Round(time.Second).String(),
	)
}

func (c *Controller) monitor(ctx context.Context) {
	interval := c.cfg.Runner.MonitorInterval.Duration()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.reconcile(ctx)
			c.claims.gc(time.Now())
		}
	}
}

func (c *Controller) reconcile(ctx context.Context) {
	for _, sess := range c.sessions.snapshot() {
		handle := sess.currentHandle()
		if handle == nil {
			continue
		}
		age := time.Since(handle.StartedAt())
		scope := c.scopeFor(sess.job)

		status, err := c.api.GetJobStatus(ctx, scope, sess.job.ID)
		if err != nil {
			c.log.Warn("failed to fetch job status", "job_id", sess.job.ID, "runner", sess.runnerName, "error", err)
			if age > c.maxLifetime() {
				c.log.Warn("job exceeded maximum lifetime, stopping", "job_id", sess.job.ID, "runner", sess.runnerName)
				sess.requestStop()
			}
			continue
		}

		switch status.Status {
		case "completed":
			sess.requestStop()
		case "in_progress":
			if status.RunnerName != "" && status.RunnerName != sess.runnerName {
				c.log.Warn("job was assigned to a different runner, stopping idle vm",
					"job_id", sess.job.ID, "runner", sess.runnerName, "assigned", status.RunnerName)
				sess.requestStop()
			} else if age > c.cfg.Runner.JobTimeout.Duration() {
				c.log.Warn("job timeout exceeded, stopping vm", "job_id", sess.job.ID, "runner", sess.runnerName)
				sess.requestStop()
			}
		case "queued":
			if status.RunnerName == "" && age > c.cfg.Runner.RegistrationTimeout.Duration() {
				c.log.Warn("runner did not pick up the job, stopping idle vm",
					"job_id", sess.job.ID, "runner", sess.runnerName)
				sess.requestStop()
			}
		}

		if age > c.maxLifetime() {
			c.log.Warn("microvm exceeded maximum lifetime, stopping", "job_id", sess.job.ID, "runner", sess.runnerName)
			sess.requestStop()
		}
	}
}

func (c *Controller) maxLifetime() time.Duration {
	return c.cfg.Runner.BootTimeout.Duration() +
		c.cfg.Runner.RegistrationTimeout.Duration() +
		c.cfg.Runner.JobTimeout.Duration()
}

func (c *Controller) scopeFor(job *Job) githubapp.Scope {
	return githubapp.Scope{Repo: job.Repo, Org: job.Org, InstallationID: job.InstallationID}
}

func (c *Controller) runnerName(jobID int64) string {
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	name := fmt.Sprintf("%s-j%d-%s", c.cfg.Runner.NamePrefix, jobID, suffix)
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func (c *Controller) allowed(repo githubapp.Repo, org string) bool {
	if len(c.cfg.GitHub.Repositories) == 0 && len(c.cfg.GitHub.Organizations) == 0 {
		return true
	}
	if len(c.cfg.GitHub.Repositories) > 0 {
		for _, allowed := range c.cfg.GitHub.Repositories {
			if strings.EqualFold(strings.TrimSpace(allowed), repo.String()) {
				return true
			}
		}
	}
	if len(c.cfg.GitHub.Organizations) > 0 {
		for _, allowed := range c.cfg.GitHub.Organizations {
			if org != "" && strings.EqualFold(strings.TrimSpace(allowed), org) {
				return true
			}
		}
	}
	return false
}

// autoLabels are added to every self-hosted runner by the Actions runner
// itself. Passing them to generate-jitconfig is unnecessary.
var autoLabels = map[string]struct{}{
	"self-hosted": {},
	"linux":       {},
	"x64":         {},
	"arm64":       {},
	"windows":     {},
	"macos":       {},
}

// JITLabels strips the labels the runner adds automatically. GitHub requires at
// least one custom label, so the input is returned unchanged when filtering
// would leave nothing.
func JITLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if _, ok := autoLabels[strings.ToLower(strings.TrimSpace(l))]; ok {
			continue
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return append([]string(nil), labels...)
	}
	return out
}

// MatchLabels reports whether a job can run on a runner with the given labels.
// Every require label must appear on the job, and every job label must be
// provided by the runner (GitHub label matching is case-insensitive).
func MatchLabels(jobLabels, runnerLabels, requireLabels []string) bool {
	runner := make(map[string]struct{}, len(runnerLabels))
	for _, l := range runnerLabels {
		runner[strings.ToLower(strings.TrimSpace(l))] = struct{}{}
	}
	job := make(map[string]struct{}, len(jobLabels))
	for _, l := range jobLabels {
		job[strings.ToLower(strings.TrimSpace(l))] = struct{}{}
	}
	for _, l := range requireLabels {
		if _, ok := job[strings.ToLower(strings.TrimSpace(l))]; !ok {
			return false
		}
	}
	for l := range job {
		if _, ok := runner[l]; !ok {
			return false
		}
	}
	return true
}

// claimSet prevents two deliveries for the same job from starting two VMs.
type claimSet struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[int64]time.Time
}

func newClaimSet(ttl time.Duration) *claimSet {
	return &claimSet{ttl: ttl, m: make(map[int64]time.Time)}
}

func (c *claimSet) acquire(id int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[id]; ok {
		return false
	}
	c.m[id] = time.Now()
	return true
}

func (c *claimSet) release(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}

func (c *claimSet) gc(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, at := range c.m {
		if now.Sub(at) > c.ttl {
			delete(c.m, id)
		}
	}
}

// sessionRegistry indexes running sessions by job and runner name.
type sessionRegistry struct {
	mu       sync.Mutex
	byJob    map[int64]*session
	byRunner map[string]*session
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{byJob: make(map[int64]*session), byRunner: make(map[string]*session)}
}

func (r *sessionRegistry) add(s *session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byJob[s.job.ID] = s
	r.byRunner[s.runnerName] = s
}

func (r *sessionRegistry) remove(s *session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byJob, s.job.ID)
	delete(r.byRunner, s.runnerName)
}

func (r *sessionRegistry) byJobID(id int64) (*session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byJob[id]
	return s, ok
}

func (r *sessionRegistry) byRunnerName(name string) (*session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byRunner[name]
	return s, ok
}

func (r *sessionRegistry) snapshot() []*session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*session, 0, len(r.byJob))
	for _, s := range r.byJob {
		out = append(out, s)
	}
	return out
}

// session is one dispatch attempt: a JIT runner and (once booted) its VM.
type session struct {
	job        *Job
	runnerName string
	log        *slog.Logger

	mu            sync.Mutex
	handle        vm.Handle
	stopRequested bool
}

func newSession(job *Job, runnerName string, log *slog.Logger) *session {
	return &session{job: job, runnerName: runnerName, log: log}
}

func (s *session) attach(handle vm.Handle) {
	s.mu.Lock()
	s.handle = handle
	stop := s.stopRequested
	s.mu.Unlock()
	if stop {
		go s.stopHandle(handle)
	}
}

func (s *session) currentHandle() vm.Handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handle
}

func (s *session) handleID() string {
	if h := s.currentHandle(); h != nil {
		return h.ID()
	}
	return ""
}

func (s *session) stopWasRequested() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopRequested
}

func (s *session) requestStop() {
	s.mu.Lock()
	if s.stopRequested {
		s.mu.Unlock()
		return
	}
	s.stopRequested = true
	handle := s.handle
	s.mu.Unlock()
	if handle != nil {
		go s.stopHandle(handle)
	}
}

func (s *session) stopHandle(handle vm.Handle) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := handle.Stop(ctx); err != nil {
		s.log.Warn("stop microvm", "runner", s.runnerName, "error", err)
	}
}
