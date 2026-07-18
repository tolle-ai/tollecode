package etl

import (
	"path/filepath"
	"sync"

	chromem "github.com/philippgille/chromem-go"
)

const collectionName = "knowledge"

var (
	storeMu sync.Mutex
	stores  = map[string]*chromem.DB{}
)

// KnowledgeDir returns the path where a workspace's vector DB is persisted.
func KnowledgeDir(workspace string) string {
	return filepath.Join(workspace, ".agent", "knowledge")
}

// OpenDB returns (or opens) the persistent chromem-go DB for a workspace.
// Multiple calls for the same workspace return the same instance.
func OpenDB(workspace string) (*chromem.DB, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	if db, ok := stores[workspace]; ok {
		return db, nil
	}
	db, err := chromem.NewPersistentDB(KnowledgeDir(workspace), false)
	if err != nil {
		return nil, err
	}
	stores[workspace] = db
	return db, nil
}

// CloseDB evicts the cached DB for a workspace (e.g. after workspace close).
func CloseDB(workspace string) {
	storeMu.Lock()
	delete(stores, workspace)
	storeMu.Unlock()
}
