package proxy

import (
	"log"
	"time"

	"github.com/missdeer/aiproxy/config"
	"github.com/missdeer/aiproxy/oauthcache"
)

var authManagers = map[config.APIType]oauthcache.Refresher{
	config.APITypeCodex:       codexAuthManager,
	config.APITypeClaudeCode:  claudeCodeAuthManager,
	config.APITypeGeminiCLI:   geminiCLIAuthManager,
	config.APITypeAntigravity: antigravityAuthManager,
	config.APITypeKiro:        kiroAuthManager,
}

func StartProactiveRefresh(getCfg func() *config.Config, stop <-chan struct{}) {
	log.Println("[REFRESH] Starting proactive token refresh")
	refreshAllManagers(getCfg())

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			log.Println("[REFRESH] Proactive token refresh stopped")
			return
		case <-ticker.C:
			refreshAllManagers(getCfg())
		}
	}
}

func refreshAllManagers(cfg *config.Config) {
	if cfg == nil {
		return
	}
	timeout := time.Duration(cfg.UpstreamRequestTimeout) * time.Second
	for _, u := range cfg.Upstreams {
		if !u.IsEnabled() || len(u.AuthFiles) == 0 {
			continue
		}
		if mgr, ok := authManagers[u.APIType]; ok {
			mgr.RefreshAll(u.AuthFiles, timeout)
		}
	}
}
