package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/missdeer/aiproxy/config"
	"github.com/missdeer/aiproxy/proxy"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

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

	handler := proxy.NewHandler(cfg)
	openaiHandler := proxy.NewOpenAIHandler(cfg)
	responsesHandler := proxy.NewResponsesHandler(cfg)
	geminiHandler := proxy.NewGeminiCompatHandler(cfg)

	http.Handle("/v1/messages", handler)
	http.Handle("/v1/chat/completions", openaiHandler)
	http.Handle("/v1/responses", responsesHandler)
	http.Handle("/v1beta/models/", geminiHandler)
	http.Handle("/v1/models/", geminiHandler)

	addr := cfg.Bind + cfg.Listen
	log.Printf("Starting proxy server on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
