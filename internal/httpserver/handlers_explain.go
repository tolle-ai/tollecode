package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tolle-ai/tollecode/internal/ai"
)

type explainRequest struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
	Symbol     string `json:"symbol"`
	Context    string `json:"context"`
	Language   string `json:"language"`
	MaxTokens  int    `json:"max_tokens"`
}

type explainResponse struct {
	Explanation string `json:"explanation"`
	Error       string `json:"error,omitempty"`
}

func handleExplain(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req explainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(explainResponse{Error: "invalid request"})
		return
	}

	if req.MaxTokens <= 0 {
		req.MaxTokens = 256
	}
	if req.MaxTokens > 512 {
		req.MaxTokens = 512
	}

	provider := ai.Global.Get(req.ProviderID)
	if provider == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(explainResponse{Error: "provider not found: " + req.ProviderID})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	lang := req.Language
	if lang == "" {
		lang = "code"
	}

	system := "You are a concise code documentation assistant. Explain code clearly. " +
		"Use markdown: **bold** for symbol names, `backticks` for code. " +
		"Structure: one sentence summary, then bullet points for key details. " +
		"Keep the total response under 120 words."

	var userMsg string
	if req.Symbol != "" {
		userMsg = "Explain `" + req.Symbol + "` in this " + lang + " code:\n\n```" + lang + "\n" + req.Context + "\n```"
	} else {
		userMsg = "Explain this " + lang + " code:\n\n```" + lang + "\n" + req.Context + "\n```"
	}

	stream, err := provider.Stream(ctx, ai.StreamRequest{
		Model:     req.Model,
		System:    system,
		Messages:  []ai.ChatMessage{{Role: "user", Content: userMsg}},
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(explainResponse{Error: err.Error()})
		return
	}

	var sb strings.Builder
	for ev := range stream {
		switch ev.Type {
		case "token":
			sb.WriteString(ev.Text)
		case "error":
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(explainResponse{Error: ev.Err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(explainResponse{Explanation: sb.String()})
}
