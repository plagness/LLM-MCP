package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// EmbeddingRequests counts embedding proxy requests by model, device, and status.
	EmbeddingRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmcore_embedding_requests_total",
			Help: "Total embedding proxy requests",
		},
		[]string{"model", "device", "status"},
	)

	// EmbeddingDuration tracks embedding proxy latency.
	EmbeddingDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmcore_embedding_duration_seconds",
			Help:    "Embedding proxy latency in seconds",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		},
		[]string{"model", "device"},
	)

	// JobsCreated counts jobs enqueued by kind.
	JobsCreated = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmcore_jobs_created_total",
			Help: "Total jobs created",
		},
		[]string{"kind"},
	)

	// DevicesOnline tracks the number of online Ollama devices.
	DevicesOnline = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmcore_devices_online",
			Help: "Number of online Ollama devices",
		},
	)

	// DiscoveryRuns counts discovery executions.
	DiscoveryRuns = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmcore_discovery_runs_total",
			Help: "Discovery run executions",
		},
		[]string{"status"},
	)

	// DiscoveryDuration tracks discovery run duration.
	DiscoveryDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "llmcore_discovery_duration_seconds",
			Help:    "Discovery run duration in seconds",
			Buckets: []float64{0.5, 1, 2.5, 5, 10, 30},
		},
	)

	// EmbeddingInputTokens tracks total tokens processed.
	EmbeddingInputTokens = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmcore_embedding_input_tokens_total",
			Help: "Total input tokens for embedding requests",
		},
		[]string{"model", "device"},
	)

	// ChatRequests counts chat completion proxy requests.
	ChatRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmcore_chat_requests_total",
			Help: "Total chat completion proxy requests",
		},
		[]string{"model", "provider", "status"},
	)

	// ChatDuration tracks chat completion latency.
	ChatDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmcore_chat_duration_seconds",
			Help:    "Chat completion latency in seconds",
			Buckets: []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120},
		},
		[]string{"model", "provider"},
	)

	// ChatTokens tracks tokens consumed by chat completions.
	ChatTokens = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmcore_chat_tokens_total",
			Help: "Total tokens for chat completion requests",
		},
		[]string{"model", "provider", "direction"},
	)

	// ChatCost tracks estimated cost of chat completions in USD.
	ChatCost = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmcore_chat_cost_usd_total",
			Help: "Estimated cost of chat completions in USD",
		},
		[]string{"model", "provider"},
	)
)
