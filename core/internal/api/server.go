package api

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"llm-mcp/core/internal/discovery"
	"llm-mcp/core/internal/routing"
)

// Server содержит зависимости для HTTP-хендлеров
type Server struct {
	DB        *pgxpool.Pool
	Version   string
	Discovery *discovery.Runner
	Router    *routing.Router
}

// New создаёт Server
func New(db *pgxpool.Pool, version string, disc *discovery.Runner, router *routing.Router) *Server {
	return &Server{
		DB:        db,
		Version:   version,
		Discovery: disc,
		Router:    router,
	}
}

// RegisterRoutes регистрирует все HTTP-маршруты
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", s.HandleHealth)
	mux.HandleFunc("/version", s.HandleHealth)
	mux.HandleFunc("/v1/jobs", s.HandleJobs)
	mux.HandleFunc("/v1/jobs/", s.HandleJobByID)
	mux.HandleFunc("/v1/workers/register", s.HandleWorkerRegister)
	mux.HandleFunc("/v1/workers/claim", s.HandleWorkerClaim)
	mux.HandleFunc("/v1/workers/complete", s.HandleWorkerComplete)
	mux.HandleFunc("/v1/workers/fail", s.HandleWorkerFail)
	mux.HandleFunc("/v1/workers/heartbeat", s.HandleWorkerHeartbeat)
	mux.HandleFunc("/v1/devices/offline", s.HandleDeviceOffline)
	mux.HandleFunc("/v1/discovery/run", s.HandleDiscoveryRun)
	mux.HandleFunc("/v1/discovery/last", s.HandleDiscoveryLast)
	mux.HandleFunc("/v1/llm/request", s.HandleLLMRequest)
	mux.HandleFunc("/v1/benchmarks/run", s.HandleBenchmarkRun)
	mux.HandleFunc("/v1/benchmarks", s.HandleBenchmarksList)
	mux.HandleFunc("/v1/costs/summary", s.HandleCostsSummary)
	mux.HandleFunc("/v1/dashboard", s.HandleDashboard)
}
