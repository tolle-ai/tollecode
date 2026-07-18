package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tolle-ai/tollecode/internal/ai"
)

func mountProviders(r chi.Router) {
	r.Get("/providers", listProviders)
	r.Post("/providers", createProvider)
	r.Put("/providers/{id}", updateProvider)
	r.Delete("/providers/{id}", deleteProvider)
	r.Put("/providers/{id}/toggle", toggleProvider)
	r.Get("/providers/{id}/models", listProviderModels)
}

func listProviders(w http.ResponseWriter, r *http.Request) {
	ai.Global.Reload()
	writeJSON(w, ai.Global.ListForUI())
}

type providerBody struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	APIKey       string `json:"apiKey"`
	Endpoint     string `json:"endpoint"`
	BaseUrl      string `json:"baseUrl"` // UI sends baseUrl; falls back to Endpoint
	DefaultModel string `json:"defaultModel"`
	// Models can arrive as []string (desktop) or a comma-separated string (cloud UI).
	Models  json.RawMessage `json:"models"`
	Enabled *bool           `json:"enabled"`
}

func (b *providerBody) endpoint() string {
	if b.BaseUrl != "" {
		return b.BaseUrl
	}
	return b.Endpoint
}

func (b *providerBody) modelList() []string {
	if len(b.Models) == 0 {
		return nil
	}
	// Try array first.
	var arr []string
	if json.Unmarshal(b.Models, &arr) == nil {
		return arr
	}
	// Fall back to comma-separated string.
	var s string
	if json.Unmarshal(b.Models, &s) == nil {
		var out []string
		for _, m := range strings.Split(s, ",") {
			if t := strings.TrimSpace(m); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	return nil
}

func createProvider(w http.ResponseWriter, r *http.Request) {
	var body providerBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Type == "" {
		writeErr(w, http.StatusBadRequest, "type is required")
		return
	}
	if body.ID == "" {
		body.ID = fmt.Sprintf("prov-%d", time.Now().UnixMilli())
	}

	cfgs := ai.LoadAllConfigs()
	for _, c := range cfgs {
		if c.ID == body.ID {
			writeErr(w, http.StatusConflict, "provider id already exists")
			return
		}
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	cfgs = append(cfgs, ai.ProviderConfig{
		ID:           body.ID,
		Name:         body.Name,
		Type:         body.Type,
		APIKey:       body.APIKey,
		Endpoint:     body.endpoint(),
		DefaultModel: body.DefaultModel,
		Models:       stringsToModelEntries(body.modelList()),
		Enabled:      enabled,
	})
	if err := ai.Global.SaveConfigs(cfgs); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"id": body.ID})
}

func updateProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body providerBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	cfgs := ai.LoadAllConfigs()
	found := false
	for i, c := range cfgs {
		if c.ID != id {
			continue
		}
		found = true
		if body.Name != "" {
			cfgs[i].Name = body.Name
		}
		if body.Type != "" {
			cfgs[i].Type = body.Type
		}
		if body.APIKey != "" {
			cfgs[i].APIKey = body.APIKey
		}
		if ep := body.endpoint(); ep != "" {
			cfgs[i].Endpoint = ep
		}
		if ml := body.modelList(); ml != nil {
			cfgs[i].Models = stringsToModelEntries(ml)
		}
		if body.DefaultModel != "" {
			cfgs[i].DefaultModel = body.DefaultModel
		}
		if body.Enabled != nil {
			cfgs[i].Enabled = *body.Enabled
		}
		break
	}
	if !found {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	}
	if err := ai.Global.SaveConfigs(cfgs); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func deleteProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	cfgs := ai.LoadAllConfigs()
	next := cfgs[:0]
	for _, c := range cfgs {
		if c.ID != id {
			next = append(next, c)
		}
	}
	if len(next) == len(cfgs) {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	}
	if err := ai.Global.SaveConfigs(next); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toggleProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	cfgs := ai.LoadAllConfigs()
	found := false
	for i, c := range cfgs {
		if c.ID == id {
			cfgs[i].Enabled = !c.Enabled
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	}
	if err := ai.Global.SaveConfigs(cfgs); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func listProviderModels(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p := ai.Global.Get(id)
	if p == nil {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	}
	models, err := p.DiscoverModels(context.Background())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, models)
}

func stringsToModelEntries(models []string) []ai.ModelEntry {
	entries := make([]ai.ModelEntry, len(models))
	for i, m := range models {
		entries[i] = ai.ModelEntry{ID: m, Name: m}
	}
	return entries
}
