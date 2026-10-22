package controller

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/config"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/metrics"
)

// ErrQueueFull is returned when the pending job queue is at capacity.
var ErrQueueFull = errors.New("controller: job queue is full")

// scheduler serialises slot accounting on a single goroutine and starts jobs
// whose repository limit allows it.
type scheduler struct {
	log        *slog.Logger
	maxTotal   int
	maxPerRepo int
	queueSize  int
	start      func(context.Context, *Job)

	enqueueCh  chan *Job
	finishedCh chan int64

	pending []*Job
	running map[int64]string // job ID -> repository
	perRepo map[string]int
	queued  atomic.Int64
	stopped atomic.Bool
}

func newScheduler(cfg config.RunnerConfig, log *slog.Logger, start func(context.Context, *Job)) *scheduler {
	return &scheduler{
		log:        log,
		maxTotal:   cfg.MaxConcurrentJobs,
		maxPerRepo: cfg.MaxJobsPerRepo,
		queueSize:  cfg.QueueSize,
		start:      start,
		enqueueCh:  make(chan *Job, cfg.QueueSize+1),
		finishedCh: make(chan int64, cfg.MaxConcurrentJobs*4+64),
		running:    make(map[int64]string),
		perRepo:    make(map[string]int),
	}
}

// Enqueue submits a job without blocking. It returns ErrQueueFull when the
// pending queue is at capacity.
func (s *scheduler) Enqueue(job *Job) error {
	if s.stopped.Load() {
		return errors.New("controller: scheduler is stopped")
	}
	if s.queued.Add(1) > int64(s.queueSize) {
		s.queued.Add(-1)
		return ErrQueueFull
	}
	select {
	case s.enqueueCh <- job:
		metrics.QueueDepth.Set(float64(s.queued.Load()))
		return nil
	default:
		s.queued.Add(-1)
		return ErrQueueFull
	}
}

func (s *scheduler) stop() { s.stopped.Store(true) }

func (s *scheduler) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.enqueueCh:
			s.pending = append(s.pending, job)
			s.dispatch(ctx)
		case id := <-s.finishedCh:
			repo, ok := s.running[id]
			if ok {
				delete(s.running, id)
				s.perRepo[repo]--
				if s.perRepo[repo] <= 0 {
					delete(s.perRepo, repo)
				}
			}
			s.dispatch(ctx)
		}
	}
}

func (s *scheduler) dispatch(ctx context.Context) {
	for len(s.running) < s.maxTotal {
		idx := s.pickLocked()
		if idx < 0 {
			return
		}
		job := s.pending[idx]
		s.pending = append(s.pending[:idx], s.pending[idx+1:]...)

		repo := job.Repo.String()
		s.running[job.ID] = repo
		s.perRepo[repo]++
		s.queued.Add(-1)
		metrics.QueueDepth.Set(float64(s.queued.Load()))
		go s.start(ctx, job)
	}
}

// pickLocked returns the index of the first pending job whose repository still
// has capacity, or -1.
func (s *scheduler) pickLocked() int {
	for i, job := range s.pending {
		if s.maxPerRepo > 0 && s.perRepo[job.Repo.String()] >= s.maxPerRepo {
			continue
		}
		return i
	}
	return -1
}

// finish must be called exactly once per started job.
func (s *scheduler) finish(id int64) {
	s.finishedCh <- id
}
