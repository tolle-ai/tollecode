package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tolle-ai/tollecode/internal/ai"
)

const ollamaCloudBase = "https://ollama.com/api"

var webHTTPClient = &http.Client{Timeout: 30 * time.Second}

func ollamaAPIKey(cfg *Config) (string, error) {
	key := ai.Global.OllamaAPIKey()
	if key == "" {
		return "", fmt.Errorf("no Ollama API key configured — add your key at ollama.com/settings/keys to an Ollama provider in Settings → Providers")
	}
	return key, nil
}

func toolWebSearch(cfg *Config, inp map[string]any) (string, string, bool) {
	query, _ := inp["query"].(string)
	if query == "" {
		return "web_search requires a 'query' parameter", "", true
	}

	apiKey, err := ollamaAPIKey(cfg)
	if err != nil {
		return err.Error(), "", true
	}

	maxResults := 5
	if v, ok := inp["max_results"].(float64); ok && v > 0 {
		maxResults = int(v)
		if maxResults > 10 {
			maxResults = 10
		}
	}

	body, _ := json.Marshal(map[string]any{
		"query":       query,
		"max_results": maxResults,
	})

	req, err := http.NewRequest(http.MethodPost, ollamaCloudBase+"/web_search", bytes.NewReader(body))
	if err != nil {
		return "web_search: failed to build request: " + err.Error(), "", true
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := webHTTPClient.Do(req)
	if err != nil {
		return "web_search: request failed: " + err.Error(), "", true
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if resp.StatusCode != 200 {
		return fmt.Sprintf("web_search: Ollama API returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))), "", true
	}

	var result struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "web_search: failed to parse response: " + err.Error(), "", true
	}

	if len(result.Results) == 0 {
		return "No results found for: " + query, "", false
	}

	var sb strings.Builder
	for i, r := range result.Results {
		fmt.Fprintf(&sb, "## Result %d: %s\nURL: %s\n\n%s\n\n", i+1, r.Title, r.URL, r.Content)
	}
	return sb.String(), "", false
}

func toolWebFetch(cfg *Config, inp map[string]any) (string, string, bool) {
	url, _ := inp["url"].(string)
	if url == "" {
		return "web_fetch requires a 'url' parameter", "", true
	}

	apiKey, err := ollamaAPIKey(cfg)
	if err != nil {
		return err.Error(), "", true
	}

	body, _ := json.Marshal(map[string]any{"url": url})

	req, err := http.NewRequest(http.MethodPost, ollamaCloudBase+"/web_fetch", bytes.NewReader(body))
	if err != nil {
		return "web_fetch: failed to build request: " + err.Error(), "", true
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := webHTTPClient.Do(req)
	if err != nil {
		return "web_fetch: request failed: " + err.Error(), "", true
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if resp.StatusCode != 200 {
		return fmt.Sprintf("web_fetch: Ollama API returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))), "", true
	}

	var result struct {
		Title   string   `json:"title"`
		Content string   `json:"content"`
		Links   []string `json:"links"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "web_fetch: failed to parse response: " + err.Error(), "", true
	}

	var sb strings.Builder
	if result.Title != "" {
		fmt.Fprintf(&sb, "# %s\nURL: %s\n\n", result.Title, url)
	}
	sb.WriteString(result.Content)
	if len(result.Links) > 0 {
		sb.WriteString("\n\n## Links\n")
		for _, l := range result.Links {
			fmt.Fprintf(&sb, "- %s\n", l)
		}
	}
	return sb.String(), "", false
}
