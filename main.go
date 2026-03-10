package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/missdeer/aiproxy/config"
	"github.com/missdeer/aiproxy/middleware"
	"github.com/missdeer/aiproxy/proxy"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// Create configuration manager
	cfgManager, err := config.NewManager(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	cfg := cfgManager.Get()

	// Configure rotating file logging if log file is specified
	if cfg.Log.File != "" {
		log.SetOutput(&lumberjack.Logger{
			Filename:   cfg.Log.File,
			MaxSize:    cfg.Log.MaxSize,
			MaxBackups: cfg.Log.MaxBackups,
			MaxAge:     cfg.Log.MaxAge,
			Compress:   cfg.Log.Compress,
		})
	}

	log.Printf("Loaded %d upstreams", len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		log.Printf("  - %s (weight: %d)", u.Name, u.Weight)
	}

	// Create handlers
	anthropicHandler := proxy.NewAnthropicHandler(cfg)
	openaiHandler := proxy.NewOpenAIHandler(cfg)
	responsesHandler := proxy.NewResponsesHandler(cfg)
	geminiHandler := proxy.NewGeminiCompatHandler(cfg)

	// Register reload callback to update all handlers
	cfgManager.OnReload(func(newCfg *config.Config) {
		anthropicHandler.UpdateConfig(newCfg)
		openaiHandler.UpdateConfig(newCfg)
		responsesHandler.UpdateConfig(newCfg)
		geminiHandler.UpdateConfig(newCfg)
	})

	// Start watching config file for changes
	if err := cfgManager.StartWatching(); err != nil {
		log.Printf("[WARNING] Failed to start config watcher: %v", err)
	} else {
		defer cfgManager.StopWatching()
	}

	// Start proactive token refresh for OAuth-based upstreams
	stopRefresh := make(chan struct{})
	go proxy.StartProactiveRefresh(cfgManager.Get, stopRefresh)

	mux := http.NewServeMux()
	mux.Handle("POST /v1/messages", middleware.DecompressionMiddleware(anthropicHandler))
	mux.Handle("POST /v1/chat/completions", middleware.DecompressionMiddleware(openaiHandler))
	mux.Handle("POST /v1/responses", middleware.DecompressionMiddleware(responsesHandler))
	mux.Handle("POST /v1/responses/compact", middleware.DecompressionMiddleware(responsesHandler))
	mux.Handle("POST /v1beta/models/{rest...}", middleware.DecompressionMiddleware(geminiHandler))
	mux.Handle("POST /v1/models/{rest...}", middleware.DecompressionMiddleware(geminiHandler))

	addr := cfg.Bind + cfg.Listen
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Graceful shutdown: wait for in-flight requests to complete
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("Received %v, shutting down gracefully...", sig)

		close(stopRefresh)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		srv.SetKeepAlivesEnabled(false)
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("Graceful shutdown timed out, forcing: %v", err)
		}
	}()

	log.Printf("Starting proxy server on %s", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server failed: %v", err)
	}
	log.Println("Server stopped")
}
