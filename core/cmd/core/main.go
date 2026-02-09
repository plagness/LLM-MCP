package main

import (
	"context"
	"log"
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
	addr := config.Getenv("CORE_HTTP_ADDR", ":8080")
	grpcAddr := config.Getenv("CORE_GRPC_ADDR", ":9090")
	version := config.Getenv("CORE_VERSION", config.Getenv("LLM_MCP_VERSION", "0.1.0"))
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		log.Fatal("DB_DSN is required")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("db connect error: %v", err)
	}
	defer pool.Close()
	if err := ensureSchema(ctx, pool); err != nil {
		log.Printf("schema ensure error: %v", err)
	}

	disc := discovery.New(pool)
	router := routing.New(pool)
	srv := api.New(pool, version, disc, router)

	if err := limits.ApplyDeviceLimits(context.Background(), pool); err != nil {
		log.Printf("device limits apply error: %v", err)
	}
	if interval := config.GetenvInt("DEVICE_LIMITS_INTERVAL", 0); interval > 0 {
		go func() {
			ticker := time.NewTicker(time.Duration(interval) * time.Second)
			defer ticker.Stop()
			for {
				if err := limits.ApplyDeviceLimits(context.Background(), pool); err != nil {
					log.Printf("device limits apply error: %v", err)
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
		log.Printf("core http listening on %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http error: %v", err)
		}
	}()

	go func() {
		lis, err := net.Listen("tcp", grpcAddr)
		if err != nil {
			log.Fatalf("grpc listen error: %v", err)
		}
		grpcSrv := grpc.NewServer()
		pb.RegisterCoreServer(grpcSrv, grpcserver.New(pool))
		log.Printf("core grpc listening on %s", grpcAddr)
		if err := grpcSrv.Serve(lis); err != nil {
			log.Fatalf("grpc serve error: %v", err)
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
		log.Printf("shutdown error: %v", err)
	}
}

func ensureSchema(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `ALTER TABLE IF EXISTS benchmarks ADD COLUMN IF NOT EXISTS meta JSONB;`)
	return err
}
