package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tolle-ai/tollecode/internal/etl"
)

func toolSearchKnowledgeBase(ctx context.Context, cfg *Config, inp map[string]any) string {
	query, _ := inp["query"].(string)
	if query == "" {
		return "Error: 'query' is required."
	}
	topK := 5
	if v, ok := inp["top_k"].(float64); ok && v > 0 {
		topK = int(v)
	}

	results, err := etl.SearchKnowledge(ctx, cfg.Workspace, query, cfg.ProviderID, topK)
	if err != nil {
		return "Error searching knowledge base: " + err.Error()
	}
	if len(results) == 0 {
		return "No results found. The knowledge base may be empty — use ingest_document to add files first."
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d result(s) for %q:\n\n", len(results), query)
	for i, r := range results {
		source := r.Metadata["source"]
		chunk := r.Metadata["chunk_index"]
		fmt.Fprintf(&sb, "--- Result %d (source: %s, chunk: %s, similarity: %.3f) ---\n%s\n\n",
			i+1, source, chunk, r.Similarity, r.Content)
	}
	return sb.String()
}

func toolIngestDocument(ctx context.Context, cfg *Config, inp map[string]any) string {
	path, _ := inp["path"].(string)
	if path == "" {
		return "Error: 'path' is required."
	}

	// Resolve to absolute path within workspace.
	absPath := path
	if !filepath.IsAbs(path) {
		absPath = filepath.Join(cfg.Workspace, path)
	}

	n, err := etl.IngestFile(ctx, cfg.Workspace, absPath, cfg.ProviderID)
	if err != nil {
		return "Error ingesting document: " + err.Error()
	}
	if n == 0 {
		return fmt.Sprintf("Skipped %q — unsupported file type or empty file.", path)
	}
	return fmt.Sprintf("Ingested %q into the knowledge base (%d chunk(s)).", path, n)
}
