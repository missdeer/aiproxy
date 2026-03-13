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

type unavailableModelEntry struct {
	UpstreamName string  `json:"upstream_name"`
	ModelName    string  `json:"model_name"`
	TimeToReset  float64 `json:"time_to_reset"`
}

// UnavailableModelsHandler handles GET /unavailable_models requests
type UnavailableModelsHandler struct {
	balancer *balancer.WeightedRoundRobin
}

func NewUnavailableModelsHandler(bal *balancer.WeightedRoundRobin) *UnavailableModelsHandler {
	return &UnavailableModelsHandler{balancer: bal}
}

func (h *UnavailableModelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	models := h.balancer.UnavailableModels()

	entries := make([]unavailableModelEntry, len(models))
	for i, m := range models {
		entries[i] = unavailableModelEntry{
			UpstreamName: m.UpstreamName,
			ModelName:    m.ModelName,
			TimeToReset:  m.TimeToReset.Seconds(),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}

type resetModelRequest struct {
	UpstreamName string `json:"upstream_name"`
	ModelName    string `json:"model_name"`
}

type ResetModelHandler struct {
	balancer *balancer.WeightedRoundRobin
}

func NewResetModelHandler(bal *balancer.WeightedRoundRobin) *ResetModelHandler {
	return &ResetModelHandler{balancer: bal}
}

func (h *ResetModelHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req resetModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.UpstreamName == "" || req.ModelName == "" {
		http.Error(w, `{"error":"upstream_name and model_name are required"}`, http.StatusBadRequest)
		return
	}

	if !h.balancer.ResetModel(req.UpstreamName, req.ModelName) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "model is not unavailable"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
