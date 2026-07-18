package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tolle-ai/tollecode/internal/ai"
)

type autocompleteRequest struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
	Prefix     string `json:"prefix"`
	Suffix     string `json:"suffix"`
	Language   string `json:"language"`
	MaxTokens  int    `json:"max_tokens"`
	// Block context — set when the cursor is inside a detectable function/class/method.
	BlockCode string `json:"block_code"`
	BlockKind string `json:"block_kind"`
	BlockName string `json:"block_name"`
}

type autocompleteResponse struct {
	Completion string `json:"completion"`
	Error      string `json:"error,omitempty"`
}

func handleAutocomplete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req autocompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(autocompleteResponse{Error: "invalid request"})
		return
	}

	if req.MaxTokens <= 0 {
		req.MaxTokens = 128
	}
	if req.MaxTokens > 512 {
		req.MaxTokens = 512
	}

	provider := ai.Global.Get(req.ProviderID)
	if provider == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(autocompleteResponse{Error: "provider not found: " + req.ProviderID})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	lang := req.Language
	if lang == "" {
		lang = "code"
	}

	system := "You are a code completion engine. Complete the " + lang + " code at " +
		"<|fim_middle|>. Output only the inserted text — no explanations, no markdown, no code fences."

	var userMsg string
	if req.BlockCode != "" {
		// Block-aware prompt: tell the model exactly what scope it is completing inside.
		blockDesc := req.BlockKind
		if req.BlockName != "" {
			blockDesc = fmt.Sprintf("%s `%s`", req.BlockKind, req.BlockName)
		}
		userMsg = fmt.Sprintf(
			"You are completing code inside %s.\n\n"+
				"Current %s:\n```%s\n%s\n```\n\n"+
				"<|fim_prefix|>%s<|fim_suffix|>%s<|fim_middle|>",
			blockDesc, blockDesc, lang, req.BlockCode,
			req.Prefix, req.Suffix,
		)
	} else {
		userMsg = "<|fim_prefix|>" + req.Prefix + "<|fim_suffix|>" + req.Suffix + "<|fim_middle|>"
	}

	stream, err := provider.Stream(ctx, ai.StreamRequest{
		Model:     req.Model,
		System:    system,
		Messages:  []ai.ChatMessage{{Role: "user", Content: userMsg}},
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(autocompleteResponse{Error: err.Error()})
		return
	}

	var sb strings.Builder
	for ev := range stream {
		switch ev.Type {
		case "token":
			sb.WriteString(ev.Text)
		case "error":
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(autocompleteResponse{Error: ev.Err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(autocompleteResponse{Completion: sb.String()})
}
