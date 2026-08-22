package events

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// PRD §17.2's streaming metrics — Collector handles registered via
// promauto, per RULE F5; not mutable application state (CLAUDE.md
// §5.2) for the same reason every other package's metrics.go gives.
var (
	sseSubscribers = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "anvil_sse_subscribers",
		Help: "Open SSE connections across every job, from this process's Hub.",
	})

	sseEventsPublishedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "anvil_sse_events_published_total",
		Help: "Events fanned out to subscribers, by event type.",
	}, []string{"type"})

	sseSubscriberDropsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "anvil_sse_subscriber_drops_total",
		Help: "Events dropped because a subscriber's buffer was full (FR-054's stream_gap path).",
	})

	sseDeliveryLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "anvil_sse_delivery_latency_seconds",
		Help:    "Time from an event's creation to Hub fanning it out to subscribers.",
		Buckets: prometheus.DefBuckets,
	})
)
