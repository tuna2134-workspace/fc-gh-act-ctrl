// Package metrics declares the Prometheus metrics exposed by the controller.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Webhooks counts deliveries by action and result.
	Webhooks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fcghact_webhooks_total",
		Help: "GitHub webhook deliveries processed, by action and result.",
	}, []string{"action", "result"})

	// JobsDispatched counts job dispatches per repository.
	JobsDispatched = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fcghact_jobs_dispatched_total",
		Help: "Workflow jobs dispatched to a microVM, by repository.",
	}, []string{"repository"})

	// JobFailures counts failures by stage.
	JobFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fcghact_job_failures_total",
		Help: "Job lifecycle failures, by stage.",
	}, []string{"stage"})

	// JobsFinished counts finished jobs by result.
	JobsFinished = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fcghact_jobs_finished_total",
		Help: "Finished jobs by result.",
	}, []string{"result"})

	// VMsActive is the number of running microVMs.
	VMsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fcghact_vms_active",
		Help: "Number of microVMs currently running.",
	})

	// VMStartSeconds observes microVM boot latency.
	VMStartSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fcghact_vm_start_seconds",
		Help:    "Time from dispatch to a booted microVM.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	})

	// JobSeconds observes how long a microVM lived for one job.
	JobSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fcghact_vm_lifetime_seconds",
		Help:    "MicroVM lifetime from boot to exit.",
		Buckets: []float64{30, 60, 120, 300, 600, 1800, 3600, 7200},
	})

	// QueueDepth is the number of jobs waiting for a slot.
	QueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fcghact_queue_depth",
		Help: "Number of jobs waiting for an execution slot.",
	})
)
