package stdio

import (
	"context"
	"os"
	"path/filepath"

	"github.com/tolle-ai/tollecode/internal/ai"
	"github.com/tolle-ai/tollecode/internal/etl"
)

// handleKnowledgeIngest ingests a file or directory into the workspace knowledge base.
// cmd: { type: "knowledge_ingest", path: string, provider_id?: string }
func handleKnowledgeIngest(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	if ws == "" {
		Emit(map[string]any{"type": "knowledge_error", "message": "no workspace set"})
		return
	}
	path, _ := cmd["path"].(string)
	if path == "" {
		Emit(map[string]any{"type": "knowledge_error", "message": "'path' is required"})
		return
	}
	providerID := providerIDFor(state, cmd)

	absPath := path
	if !filepath.IsAbs(path) {
		absPath = filepath.Join(ws, path)
	}

	go func() {
		ctx := context.Background()
		info, err := os.Stat(absPath)
		if err != nil {
			Emit(map[string]any{"type": "knowledge_error", "message": "path not found: " + path})
			return
		}

		var totalFiles, totalChunks int
		if info.IsDir() {
			f, c, err := etl.IngestWorkspace(ctx, absPath, providerID)
			if err != nil {
				Emit(map[string]any{"type": "knowledge_error", "message": err.Error()})
				return
			}
			totalFiles, totalChunks = f, c
		} else {
			n, err := etl.IngestFile(ctx, ws, absPath, providerID)
			if err != nil {
				Emit(map[string]any{"type": "knowledge_error", "message": err.Error()})
				return
			}
			totalFiles, totalChunks = 1, n
		}

		Emit(map[string]any{
			"type":   "knowledge_ingested",
			"path":   path,
			"files":  totalFiles,
			"chunks": totalChunks,
		})
	}()
}

// handleKnowledgeIngestWorkspace re-indexes the entire workspace.
// cmd: { type: "knowledge_ingest_workspace", provider_id?: string }
func handleKnowledgeIngestWorkspace(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	if ws == "" {
		Emit(map[string]any{"type": "knowledge_error", "message": "no workspace set"})
		return
	}
	providerID := providerIDFor(state, cmd)

	go func() {
		ctx := context.Background()
		files, chunks, err := etl.IngestWorkspace(ctx, ws, providerID)
		if err != nil {
			Emit(map[string]any{"type": "knowledge_error", "message": err.Error()})
			return
		}
		Emit(map[string]any{
			"type":   "knowledge_ingested",
			"path":   ws,
			"files":  files,
			"chunks": chunks,
		})
	}()
}

// handleKnowledgeList returns all indexed sources for the current workspace.
// cmd: { type: "knowledge_list" }
func handleKnowledgeList(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	if ws == "" {
		Emit(map[string]any{"type": "knowledge_list", "sources": []any{}})
		return
	}
	sources := etl.ListSources(ws)
	out := make([]map[string]any, len(sources))
	for i, s := range sources {
		out[i] = map[string]any{
			"source":     s.Source,
			"type":       s.Type,
			"chunks":     s.Chunks,
			"ingestedAt": s.IngestedAt,
		}
	}
	Emit(map[string]any{"type": "knowledge_list", "sources": out})
}

// handleKnowledgeDelete removes a source from the knowledge base.
// cmd: { type: "knowledge_delete", source: string, provider_id?: string }
func handleKnowledgeDelete(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	if ws == "" {
		Emit(map[string]any{"type": "knowledge_error", "message": "no workspace set"})
		return
	}
	source, _ := cmd["source"].(string)
	if source == "" {
		Emit(map[string]any{"type": "knowledge_error", "message": "'source' is required"})
		return
	}
	providerID := providerIDFor(state, cmd)

	go func() {
		ctx := context.Background()
		if err := etl.DeleteSource(ctx, ws, source, providerID); err != nil {
			Emit(map[string]any{"type": "knowledge_error", "message": err.Error()})
			return
		}
		Emit(map[string]any{"type": "knowledge_deleted", "source": source})
	}()
}

// handleKnowledgeSearch runs a semantic query against the knowledge base.
// cmd: { type: "knowledge_search", query: string, top_k?: int, provider_id?: string }
func handleKnowledgeSearch(state *ServerState, cmd map[string]any) {
	ws := state.Workspace
	if ws == "" {
		Emit(map[string]any{"type": "knowledge_error", "message": "no workspace set"})
		return
	}
	query, _ := cmd["query"].(string)
	if query == "" {
		Emit(map[string]any{"type": "knowledge_error", "message": "'query' is required"})
		return
	}
	topK := 5
	if v, ok := cmd["top_k"].(float64); ok && v > 0 {
		topK = int(v)
	}
	providerID := providerIDFor(state, cmd)

	go func() {
		ctx := context.Background()
		results, err := etl.SearchKnowledge(ctx, ws, query, providerID, topK)
		if err != nil {
			Emit(map[string]any{"type": "knowledge_error", "message": err.Error()})
			return
		}
		out := make([]map[string]any, len(results))
		for i, r := range results {
			out[i] = map[string]any{
				"content":    r.Content,
				"source":     r.Metadata["source"],
				"chunkIndex": r.Metadata["chunk_index"],
				"similarity": r.Similarity,
			}
		}
		Emit(map[string]any{"type": "knowledge_search_results", "query": query, "results": out})
	}()
}

// providerIDFor returns the provider_id from the command or falls back to the
// first available provider.
func providerIDFor(state *ServerState, cmd map[string]any) string {
	if id, ok := cmd["provider_id"].(string); ok && id != "" {
		return id
	}
	ids := ai.Global.IDs()
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}
