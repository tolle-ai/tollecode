package etl

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	chromem "github.com/philippgille/chromem-go"
)

// SourceEntry records one ingested source in the knowledge index.
type SourceEntry struct {
	Source     string `json:"source"`
	Type       string `json:"type"` // "file" | "document"
	Chunks     int    `json:"chunks"`
	IngestedAt string `json:"ingestedAt"`
}

var indexMu sync.Mutex

// IngestFile ingests a single file (absolute path) into the workspace knowledge base.
// Returns the number of chunks added, or 0 if the file type is unsupported.
func IngestFile(ctx context.Context, workspace, absPath, providerID string) (int, error) {
	text, err := Extract(absPath)
	if err != nil {
		return 0, err
	}
	if text == "" {
		return 0, nil
	}

	rel, err := filepath.Rel(workspace, absPath)
	if err != nil {
		rel = filepath.Base(absPath)
	}

	n, err := ingest(ctx, workspace, rel, text, "file", providerID)
	if err != nil {
		return 0, err
	}
	saveSourceEntry(workspace, SourceEntry{Source: rel, Type: "file", Chunks: n, IngestedAt: time.Now().UTC().Format(time.RFC3339)})
	return n, nil
}

// IngestText ingests arbitrary text (e.g. a user-uploaded document) under a given name.
func IngestText(ctx context.Context, workspace, name, text, providerID string) (int, error) {
	if text == "" {
		return 0, nil
	}
	n, err := ingest(ctx, workspace, name, text, "document", providerID)
	if err != nil {
		return 0, err
	}
	saveSourceEntry(workspace, SourceEntry{Source: name, Type: "document", Chunks: n, IngestedAt: time.Now().UTC().Format(time.RFC3339)})
	return n, nil
}

// SearchKnowledge performs a semantic search over the workspace knowledge base.
func SearchKnowledge(ctx context.Context, workspace, query, providerID string, topK int) ([]chromem.Result, error) {
	db, err := OpenDB(workspace)
	if err != nil {
		return nil, err
	}
	ef, err := EmbeddingFuncFor(providerID)
	if err != nil {
		return nil, err
	}
	col := db.GetCollection(collectionName, ef)
	if col == nil || col.Count() == 0 {
		return nil, nil
	}
	if topK <= 0 {
		topK = 5
	}
	if topK > 20 {
		topK = 20
	}
	return col.Query(ctx, query, topK, nil, nil)
}

// DeleteSource removes all chunks for a given source from the knowledge base.
func DeleteSource(ctx context.Context, workspace, source, providerID string) error {
	db, err := OpenDB(workspace)
	if err != nil {
		return err
	}
	ef, err := EmbeddingFuncFor(providerID)
	if err != nil {
		return err
	}
	col := db.GetCollection(collectionName, ef)
	if col == nil {
		return nil
	}
	if err := col.Delete(ctx, map[string]string{"source": source}, nil); err != nil {
		return err
	}
	removeSourceEntry(workspace, source)
	return nil
}

// ListSources returns all indexed sources for a workspace.
func ListSources(workspace string) []SourceEntry {
	data, err := os.ReadFile(sourcesIndexPath(workspace))
	if err != nil {
		return nil
	}
	var entries []SourceEntry
	_ = json.Unmarshal(data, &entries)
	return entries
}

// IngestWorkspace walks the workspace and ingests all supported files.
// Returns the total chunks added and a count of files processed.
func IngestWorkspace(ctx context.Context, workspace, providerID string) (files, chunks int, err error) {
	skipDirs := map[string]bool{
		"node_modules": true, ".git": true, ".agent": true,
		"vendor": true, "dist": true, "build": true, ".next": true, "__pycache__": true,
	}
	err = filepath.WalkDir(workspace, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, ferr := IngestFile(ctx, workspace, path, providerID)
		if ferr != nil {
			return nil // skip failed files
		}
		if n > 0 {
			files++
			chunks += n
		}
		return nil
	})
	return files, chunks, err
}

// ── internals ────────────────────────────────────────────────────────────────

func ingest(ctx context.Context, workspace, source, text, docType, providerID string) (int, error) {
	db, err := OpenDB(workspace)
	if err != nil {
		return 0, err
	}
	ef, err := EmbeddingFuncFor(providerID)
	if err != nil {
		return 0, err
	}
	col, err := db.GetOrCreateCollection(collectionName, nil, ef)
	if err != nil {
		return 0, err
	}

	// Delete stale chunks for this source before re-ingesting.
	_ = col.Delete(ctx, map[string]string{"source": source}, nil)

	rawChunks := Chunk(text)
	docs := make([]chromem.Document, len(rawChunks))
	for i, chunk := range rawChunks {
		docs[i] = chromem.Document{
			ID:      chunkID(source, i),
			Content: chunk,
			Metadata: map[string]string{
				"source":      source,
				"chunk_index": fmt.Sprintf("%d", i),
				"type":        docType,
			},
		}
	}
	if err := col.AddDocuments(ctx, docs, 4); err != nil {
		return 0, err
	}
	return len(docs), nil
}

func chunkID(source string, idx int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", source, idx)))
	return fmt.Sprintf("%x", h[:8])
}

func sourcesIndexPath(workspace string) string {
	return filepath.Join(KnowledgeDir(workspace), "sources.json")
}

func saveSourceEntry(workspace string, entry SourceEntry) {
	indexMu.Lock()
	defer indexMu.Unlock()

	entries := readSourcesLocked(workspace)
	// Replace existing entry for this source.
	replaced := false
	for i, e := range entries {
		if e.Source == entry.Source {
			entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	writeSourcesLocked(workspace, entries)
}

func removeSourceEntry(workspace, source string) {
	indexMu.Lock()
	defer indexMu.Unlock()

	entries := readSourcesLocked(workspace)
	filtered := entries[:0]
	for _, e := range entries {
		if !strings.EqualFold(e.Source, source) {
			filtered = append(filtered, e)
		}
	}
	writeSourcesLocked(workspace, filtered)
}

func readSourcesLocked(workspace string) []SourceEntry {
	data, err := os.ReadFile(sourcesIndexPath(workspace))
	if err != nil {
		return nil
	}
	var entries []SourceEntry
	_ = json.Unmarshal(data, &entries)
	return entries
}

func writeSourcesLocked(workspace string, entries []SourceEntry) {
	_ = os.MkdirAll(KnowledgeDir(workspace), 0o755)
	data, _ := json.MarshalIndent(entries, "", "  ")
	_ = os.WriteFile(sourcesIndexPath(workspace), data, 0o644)
}
