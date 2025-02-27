// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/thanos-io/thanos/blob/main/pkg/gate/gate.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Thanos Authors.
// Provenance-includes-location: https://github.com/prometheus/prometheus/blob/main/util/gate/gate.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Prometheus Authors.

package gate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var ErrMaxConcurrent = errors.New("max concurrent requests inflight")

// Gate controls the maximum number of concurrently running and waiting queries.
//
// Example of use:
//
//	g := gate.New(r, 5)
//
//	if err := g.Start(ctx); err != nil {
//	   return
//	}
//	defer g.Done()
type Gate interface {
	// Start initiates a new request and waits until it's our turn to fulfill a request.
	Start(ctx context.Context) error
	// Done finishes a query.
	Done()
}

// NewNoop creates a Gate implementation that doesn't enforce any limit.
func NewNoop() Gate {
	return noopGate{}
}

type noopGate struct{}

func (noopGate) Start(context.Context) error { return nil }
func (noopGate) Done()                       {}

// NewInstrumented wraps a Gate implementation with one that records max number of inflight
// requests, currently inflight requests, and the duration of calls to the Start method.
func NewInstrumented(reg prometheus.Registerer, maxConcurrent int, gate Gate) Gate {
	g := &instrumentedGate{
		gate: gate,
		max: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Name: "gate_queries_concurrent_max",
			Help: "Number of maximum concurrent queries allowed.",
		}),
		inflight: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Name: "gate_queries_in_flight",
			Help: "Number of queries that are currently in flight.",
		}),
		duration: promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
			Name:    "gate_duration_seconds",
			Help:    "How many seconds it took for queries to wait at the gate.",
			Buckets: []float64{0.01, 0.1, 0.3, 0.6, 1, 3, 6, 9, 20, 30, 60, 90, 120, 240, 360, 720},
		}),
	}

	g.max.Set(float64(maxConcurrent))
	return g
}

type instrumentedGate struct {
	gate Gate

	max      prometheus.Gauge
	inflight prometheus.Gauge
	duration prometheus.Histogram
}

func (g *instrumentedGate) Start(ctx context.Context) error {
	start := time.Now()
	defer func() {
		g.duration.Observe(time.Since(start).Seconds())
	}()

	err := g.gate.Start(ctx)
	if err != nil {
		return err
	}

	g.inflight.Inc()
	return nil
}

func (g *instrumentedGate) Done() {
	g.inflight.Dec()
	g.gate.Done()
}

func NewRejecting(maxConcurrent int) Gate {
	return &rejectingGate{
		ch: make(chan struct{}, maxConcurrent),
	}
}

type rejectingGate struct {
	ch chan struct{}
}

func (g *rejectingGate) Start(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case g.ch <- struct{}{}:
		return nil
	default:
		return fmt.Errorf("%w: %d", ErrMaxConcurrent, len(g.ch))
	}
}

func (g *rejectingGate) Done() {
	select {
	case <-g.ch:
	default:
		panic("gate.Done: more operations done than started")
	}
}

func NewBlocking(maxConcurrent int) Gate {
	return &blockingGate{
		ch: make(chan struct{}, maxConcurrent),
	}
}

type blockingGate struct {
	ch chan struct{}
}

func (g *blockingGate) Start(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case g.ch <- struct{}{}:
		return nil
	}
}

func (g *blockingGate) Done() {
	select {
	case <-g.ch:
	default:
		panic("gate.Done: more operations done than started")
	}
}
