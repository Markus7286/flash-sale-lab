// Package metrics exposes the Prometheus view of the service. It is the only
// package that imports a metrics library: internal/flashsale stays unaware that
// it is being observed.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"flash-sale/internal/flashsale"
)

const namespace = "flash_sale"

var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "requests_total",
		Help:      "Reservation requests by backend and outcome.",
	}, []string{"backend", "result"})

	// DefBuckets starts at 5ms, which is above the redis-lua p99 of 4ms: every
	// interesting sample would land in the first bucket and the quantiles would
	// be meaningless. These span the observed range of all three backends.
	duration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "request_duration_seconds",
		Help:      "Time spent handling a reservation request, in seconds.",
		Buckets:   []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
	}, []string{"backend"})
)

// ObserveReserve records one reservation attempt. The result is a
// flashsale.Status name, or one of "error", "bad_request", "unknown_backend".
func ObserveReserve(backend, result string, d time.Duration) {
	requests.WithLabelValues(backend, result).Inc()
	duration.WithLabelValues(backend).Observe(d.Seconds())
}

// RegisterWorker publishes the counters the consumer and reconciler already keep.
// CounterFunc reads those atomics rather than duplicating the bookkeeping, which
// is why neither type needed to learn about Prometheus.
func RegisterWorker(consumer *flashsale.Consumer, reconciler *flashsale.Reconciler) {
	promauto.NewCounterFunc(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "compensation_total",
		Help:      "Reservations given back to stock after a permanent failure.",
	}, func() float64 { return float64(consumer.Compensations()) })

	promauto.NewCounterFunc(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reconcile_mismatch_total",
		Help:      "Confirmed reconciliation mismatches since this worker started.",
	}, func() float64 { return float64(reconciler.Mismatches()) })
}
