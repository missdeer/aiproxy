package proxy

import (
	"encoding/json"
	"net/http"

	"github.com/missdeer/aiproxy/balancer"
)

// ModelsHandler handles GET /models requests
type ModelsHandler struct {
	balancer *balancer.WeightedRoundRobin
}

// NewModelsHandler creates a new models handler
func NewModelsHandler(bal *balancer.WeightedRoundRobin) *ModelsHandler {
	return &ModelsHandler{
		balancer: bal,
	}
}

// ServeHTTP returns available models as JSON array
func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	models := h.balancer.AvailableModels()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(models); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}
