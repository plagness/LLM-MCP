package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"llm-mcp/core/internal/api"
	"llm-mcp/core/internal/config"
	"llm-mcp/core/internal/discovery"
	"llm-mcp/core/internal/grpcserver"
	"llm-mcp/core/internal/limits"
	"llm-mcp/core/internal/pb"
	"llm-mcp/core/internal/routing"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	addr := config.Getenv("CORE_HTTP_ADDR", ":8080")
	grpcAddr := config.Getenv("CORE_GRPC_ADDR", ":9090")
	version := config.Getenv("CORE_VERSION", config.Getenv("LLM_MCP_VERSION", "0.1.0"))
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		slog.Error("DB_DSN is required")
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		slog.Error("db connect error", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := ensureSchema(ctx, pool); err != nil {
		slog.Warn("schema ensure error", "error", err)
	}

	disc := discovery.New(pool)
	router := routing.New(pool)
	srv := api.New(pool, version, disc, router)

	if err := limits.ApplyDeviceLimits(context.Background(), pool); err != nil {
		slog.Warn("device limits apply error", "error", err)
	}
	if interval := config.GetenvInt("DEVICE_LIMITS_INTERVAL", 0); interval > 0 {
		go func() {
			ticker := time.NewTicker(time.Duration(interval) * time.Second)
			defer ticker.Stop()
			for {
				if err := limits.ApplyDeviceLimits(context.Background(), pool); err != nil {
					slog.Warn("device limits apply error", "error", err)
				}
				<-ticker.C
			}
		}()
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("core http listening", "component", "http", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http error", "error", err)
			os.Exit(1)
		}
	}()

	go func() {
		lis, err := net.Listen("tcp", grpcAddr)
		if err != nil {
			slog.Error("grpc listen error", "error", err)
			os.Exit(1)
		}
		grpcSrv := grpc.NewServer()
		pb.RegisterCoreServer(grpcSrv, grpcserver.New(pool))
		slog.Info("core grpc listening", "component", "grpc", "addr", grpcAddr)
		if err := grpcSrv.Serve(lis); err != nil {
			slog.Error("grpc serve error", "error", err)
			os.Exit(1)
		}
	}()

	if interval := config.Getenv("DISCOVERY_INTERVAL", "0"); interval != "0" {
		if secs, err := strconv.Atoi(interval); err == nil && secs > 0 {
			go func() {
				ticker := time.NewTicker(time.Duration(secs) * time.Second)
				defer ticker.Stop()
				for {
					_ = disc.Run(context.Background())
					<-ticker.C
				}
			}()
		}
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
}

func ensureSchema(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `ALTER TABLE IF EXISTS benchmarks ADD COLUMN IF NOT EXISTS meta JSONB;`)
	return err
}
