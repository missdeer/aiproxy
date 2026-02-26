package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/missdeer/aiproxy/config"
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

	http.Handle("/v1/messages", anthropicHandler)
	http.Handle("/v1/chat/completions", openaiHandler)
	http.Handle("/v1/responses", responsesHandler)
	http.Handle("/v1beta/models/", geminiHandler)
	http.Handle("/v1/models/", geminiHandler)

	// Handle graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		cfgManager.StopWatching()
		os.Exit(0)
	}()

	addr := cfg.Bind + cfg.Listen
	log.Printf("Starting proxy server on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
