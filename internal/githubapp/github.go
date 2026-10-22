// Package githubapp wraps the GitHub App authentication and the handful of
// REST endpoints the controller needs.
package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/cenkalti/backoff/v4"
	"github.com/google/go-github/v88/github"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/config"
)

// DefaultAPIBase is the public GitHub REST API endpoint.
const DefaultAPIBase = "https://api.github.com"

// Repo identifies a GitHub repository.
type Repo struct {
	Owner string
	Name  string
}

// String renders the repository as owner/name.
func (r Repo) String() string {
	if r.Owner == "" {
		return r.Name
	}
	return r.Owner + "/" + r.Name
}

// ParseRepo parses an owner/name string.
func ParseRepo(s string) (Repo, error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repo{}, fmt.Errorf("invalid repository %q: expected owner/name", s)
	}
	return Repo{Owner: owner, Name: name}, nil
}

// Scope identifies where a runner should be registered. Repo must always be
// set for job lookups; Org is used when the controller scope is "org".
type Scope struct {
	Repo           Repo
	Org            string
	InstallationID int64
}

// JITRequest describes a runner to create via generate-jitconfig.
type JITRequest struct {
	Name          string
	Labels        []string
	RunnerGroupID int64
	WorkFolder    string
}

// JITConfig is a single-use runner configuration.
type JITConfig struct {
	Encoded    string
	RunnerID   int64
	RunnerName string
}

// JobStatus is the subset of a workflow job the controller reconciles on.
type JobStatus struct {
	ID           int64
	Status       string
	Conclusion   string
	RunnerName   string
	RunnerID     int64
	Labels       []string
	HTMLURL      string
	WorkflowName string
}

// API is the subset of the service used by the controller. It is an interface
// so tests can substitute a fake.
type API interface {
	GenerateJITConfig(ctx context.Context, scope Scope, req JITRequest) (*JITConfig, error)
	GetJobStatus(ctx context.Context, scope Scope, jobID int64) (*JobStatus, error)
}

// Service is a GitHub App client with per-installation token caching.
type Service struct {
	appID                 int64
	scope                 string
	apiBase               string
	defaultInstallationID int64
	appsTransport         *ghinstallation.AppsTransport
	appClient             *github.Client

	mu       sync.Mutex
	clients  map[int64]*github.Client
	installs map[string]int64
}

var _ API = (*Service)(nil)

// New creates a Service from configuration. The private key is loaded once and
// installation tokens are refreshed by ghinstallation as needed.
func New(cfg config.GitHubConfig) (*Service, error) {
	key, err := cfg.PrivateKeyBytes()
	if err != nil {
		return nil, err
	}
	apiBase, err := normalizeAPIBase(cfg.BaseURL)
	if err != nil {
		return nil, err
	}

	transport := http.RoundTripper(http.DefaultTransport)
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = t.Clone()
	}
	appsTransport, err := ghinstallation.NewAppsTransport(transport, cfg.AppID, key)
	if err != nil {
		return nil, fmt.Errorf("github app transport: %w", err)
	}
	appsTransport.BaseURL = apiBase

	appClient, err := newClient(&http.Client{Transport: appsTransport, Timeout: 60 * time.Second}, apiBase)
	if err != nil {
		return nil, err
	}

	scope := strings.ToLower(cfg.Scope)
	if scope == "" {
		scope = "repo"
	}
	return &Service{
		appID:                 cfg.AppID,
		scope:                 scope,
		apiBase:               apiBase,
		defaultInstallationID: cfg.InstallationID,
		appsTransport:         appsTransport,
		appClient:             appClient,
		clients:               make(map[int64]*github.Client),
		installs:              make(map[string]int64),
	}, nil
}

// normalizeAPIBase returns the REST API root (no trailing slash). For GitHub
// Enterprise Server the /api/v3 suffix is added when it is missing.
func normalizeAPIBase(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return DefaultAPIBase, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid github.base_url %q: %w", raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("invalid github.base_url %q: scheme must be http or https", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	host := strings.ToLower(u.Hostname())
	if host != "api.github.com" && !strings.HasPrefix(host, "api.") && !strings.HasSuffix(u.Path, "/api/v3") {
		u.Path += "/api/v3"
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func newClient(hc *http.Client, apiBase string) (*github.Client, error) {
	client, err := github.NewClient(
		github.WithHTTPClient(hc),
		github.WithEnterpriseURLs(apiBase+"/", apiBase+"/"),
	)
	if err != nil {
		return nil, fmt.Errorf("configure github client: %w", err)
	}
	return client, nil
}

// installationClient returns a client authenticated as the given installation.
func (s *Service) installationClient(installationID int64) (*github.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clients[installationID]; ok {
		return c, nil
	}
	tr := ghinstallation.NewFromAppsTransport(s.appsTransport, installationID)
	client, err := newClient(&http.Client{Transport: tr, Timeout: 60 * time.Second}, s.apiBase)
	if err != nil {
		return nil, err
	}
	s.clients[installationID] = client
	return client, nil
}

// ResolveInstallation determines the installation ID for a scope. Installation
// IDs are cached because webhook payloads normally carry them already.
func (s *Service) ResolveInstallation(ctx context.Context, scope Scope) (int64, error) {
	if scope.InstallationID > 0 {
		return scope.InstallationID, nil
	}
	if s.defaultInstallationID > 0 {
		return s.defaultInstallationID, nil
	}
	key := "org:" + strings.ToLower(scope.Org)
	if scope.Org == "" {
		key = "repo:" + strings.ToLower(scope.Repo.String())
	}
	s.mu.Lock()
	if id, ok := s.installs[key]; ok {
		s.mu.Unlock()
		return id, nil
	}
	s.mu.Unlock()

	var (
		inst *github.Installation
		err  error
	)
	if scope.Org != "" {
		inst, _, err = s.appClient.Apps.GetOrganizationInstallation(ctx, scope.Org)
		if err != nil {
			return 0, fmt.Errorf("get organization installation for %s: %w", scope.Org, err)
		}
	} else {
		if scope.Repo.Owner == "" || scope.Repo.Name == "" {
			return 0, errors.New("cannot resolve installation without repository or organization")
		}
		inst, _, err = s.appClient.Apps.GetRepositoryInstallation(ctx, scope.Repo.Owner, scope.Repo.Name)
		if err != nil {
			return 0, fmt.Errorf("get repository installation for %s: %w", scope.Repo, err)
		}
	}
	if inst == nil || inst.GetID() == 0 {
		return 0, fmt.Errorf("no installation found for %s", key)
	}
	s.mu.Lock()
	s.installs[key] = inst.GetID()
	s.mu.Unlock()
	return inst.GetID(), nil
}

// GenerateJITConfig creates a single-use runner configuration. Retryable GitHub
// failures (429/5xx) are retried with exponential backoff for up to 45 seconds.
func (s *Service) GenerateJITConfig(ctx context.Context, scope Scope, req JITRequest) (*JITConfig, error) {
	if req.Name == "" {
		return nil, errors.New("jit config: runner name is required")
	}
	if len(req.Labels) == 0 {
		return nil, errors.New("jit config: at least one label is required")
	}
	installationID, err := s.ResolveInstallation(ctx, scope)
	if err != nil {
		return nil, err
	}
	client, err := s.installationClient(installationID)
	if err != nil {
		return nil, err
	}

	body := &github.GenerateJITConfigRequest{
		Name:          req.Name,
		RunnerGroupID: req.RunnerGroupID,
		Labels:        req.Labels,
	}
	if req.WorkFolder != "" {
		body.WorkFolder = github.Ptr(req.WorkFolder)
	}

	var out *github.JITRunnerConfig
	op := func() error {
		var (
			cfg *github.JITRunnerConfig
			err error
		)
		if s.scope == "org" {
			if scope.Org == "" {
				return backoff.Permanent(errors.New("github.scope is org but the event has no organization"))
			}
			cfg, _, err = client.Actions.GenerateOrgJITConfig(ctx, scope.Org, body)
		} else {
			cfg, _, err = client.Actions.GenerateRepoJITConfig(ctx, scope.Repo.Owner, scope.Repo.Name, body)
		}
		if err != nil {
			if isRetryable(err) {
				return err
			}
			return backoff.Permanent(err)
		}
		out = cfg
		return nil
	}

	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 500 * time.Millisecond
	b.MaxInterval = 8 * time.Second
	b.MaxElapsedTime = 45 * time.Second
	if err := backoff.Retry(op, backoff.WithContext(b, ctx)); err != nil {
		return nil, fmt.Errorf("generate jit config for %s: %w", scope.Repo, err)
	}
	if out == nil || out.GetEncodedJITConfig() == "" {
		return nil, fmt.Errorf("generate jit config for %s: empty response", scope.Repo)
	}
	res := &JITConfig{Encoded: out.GetEncodedJITConfig()}
	if out.Runner != nil {
		res.RunnerID = out.Runner.GetID()
		res.RunnerName = out.Runner.GetName()
	}
	if res.RunnerName == "" {
		res.RunnerName = req.Name
	}
	return res, nil
}

// GetJobStatus fetches a workflow job. It never retries so the monitor loop can
// move on to the next VM quickly.
func (s *Service) GetJobStatus(ctx context.Context, scope Scope, jobID int64) (*JobStatus, error) {
	if scope.Repo.Owner == "" || scope.Repo.Name == "" {
		return nil, errors.New("get job status: repository is required")
	}
	installationID, err := s.ResolveInstallation(ctx, scope)
	if err != nil {
		return nil, err
	}
	client, err := s.installationClient(installationID)
	if err != nil {
		return nil, err
	}
	job, _, err := client.Actions.GetWorkflowJobByID(ctx, scope.Repo.Owner, scope.Repo.Name, jobID)
	if err != nil {
		return nil, fmt.Errorf("get job %d: %w", jobID, err)
	}
	return &JobStatus{
		ID:           job.GetID(),
		Status:       job.GetStatus(),
		Conclusion:   job.GetConclusion(),
		RunnerName:   job.GetRunnerName(),
		RunnerID:     job.GetRunnerID(),
		Labels:       job.Labels,
		HTMLURL:      job.GetHTMLURL(),
		WorkflowName: job.GetWorkflowName(),
	}, nil
}

// Check verifies that the App credentials work.
func (s *Service) Check(ctx context.Context) (string, error) {
	app, _, err := s.appClient.Apps.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("github app authentication: %w", err)
	}
	return app.GetSlug(), nil
}

// isRetryable reports whether err is a transient GitHub API failure.
func isRetryable(err error) bool {
	var er *github.ErrorResponse
	if errors.As(err, &er) && er.Response != nil {
		switch er.Response.StatusCode {
		case http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		}
	}
	var rl *github.RateLimitError
	if errors.As(err, &rl) {
		return true
	}
	var abuse *github.AbuseRateLimitError
	return errors.As(err, &abuse)
}
