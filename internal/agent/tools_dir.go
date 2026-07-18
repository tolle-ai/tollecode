package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "__pycache__": true,
	".venv": true, "dist": true, "build": true, ".next": true,
	"target": true, ".cache": true, "__MACOSX": true,
	".agent": true, // internal tollecode directory — not part of the user's project
}
var skipFiles = map[string]bool{".DS_Store": true}

func toolListDirectory(workspace string, inp map[string]any) string {
	rel, _ := inp["path"].(string)
	if rel == "" {
		rel = "."
	}
	base, err := safeJoin(workspace, rel)
	if err != nil {
		return "Error: " + err.Error()
	}

	depth := 1
	if v, ok := inp["depth"].(float64); ok {
		depth = int(v)
	}
	if depth < 1 {
		depth = 1
	} else if depth > 3 {
		depth = 3
	}

	limit := 200
	if v, ok := inp["limit"].(float64); ok {
		limit = int(v)
	}
	if limit < 1 {
		limit = 1
	} else if limit > 500 {
		limit = 500
	}

	offset := 0
	if v, ok := inp["offset"].(float64); ok {
		offset = int(v)
	}
	includeAll, _ := inp["include_all"].(bool)

	type entry struct {
		kind        string // "dir" | "file"
		name        string
		size        int64
		childFiles  int
		childDirs   int
		indentDepth int
	}
	var all []entry
	excludedDirs := map[string]bool{}

	var collect func(dir string, currentDepth int)
	collect = func(dir string, currentDepth int) {
		if currentDepth > depth {
			return
		}
		items, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		// Sort: dirs first, then files; all case-insensitive
		sort.Slice(items, func(i, j int) bool {
			a, b := items[i], items[j]
			if a.IsDir() != b.IsDir() {
				return a.IsDir()
			}
			return strings.ToLower(a.Name()) < strings.ToLower(b.Name())
		})

		for _, item := range items {
			name := item.Name()
			if item.IsDir() {
				if !includeAll && skipDirs[name] {
					excludedDirs[name+"/"] = true
					continue
				}
				sub := filepath.Join(dir, name)
				// count immediate children
				cf, cd := 0, 0
				if subItems, err := os.ReadDir(sub); err == nil {
					for _, si := range subItems {
						if si.IsDir() {
							if includeAll || !skipDirs[si.Name()] {
								cd++
							}
						} else {
							if includeAll || !skipFiles[si.Name()] {
								cf++
							}
						}
					}
				}
				all = append(all, entry{kind: "dir", name: name, childFiles: cf, childDirs: cd, indentDepth: currentDepth})
				if currentDepth < depth {
					collect(sub, currentDepth+1)
				}
			} else {
				if !includeAll && skipFiles[name] {
					continue
				}
				info, _ := item.Info()
				sz := int64(0)
				if info != nil {
					sz = info.Size()
				}
				all = append(all, entry{kind: "file", name: name, size: sz, indentDepth: currentDepth})
			}
		}
	}
	collect(base, 1)

	total := len(all)
	page := all
	if offset > total {
		offset = total
	}
	if offset > 0 || len(page) > limit {
		end := offset + limit
		if end > total {
			end = total
		}
		page = all[offset:end]
	} else if len(page) > limit {
		page = all[:limit]
	}

	relBase, _ := filepath.Rel(workspace, base)
	if relBase == "" {
		relBase = "."
	}
	var lines []string
	lines = append(lines, "Path: "+relBase)

	for _, e := range page {
		indent := strings.Repeat("  ", e.indentDepth-1)
		if e.kind == "dir" {
			lines = append(lines, fmt.Sprintf("%s%s/     (dir — %d files, %d dirs)", indent, e.name, e.childFiles, e.childDirs))
		} else {
			lines = append(lines, fmt.Sprintf("%s%s    %s", indent, e.name, fmtSize(e.size)))
		}
	}

	shown := offset + len(page)
	if shown < total {
		lines = append(lines, fmt.Sprintf("\n(%d of %d entries shown — use offset=%d to see next page)", len(page), total, shown))
	}
	if !includeAll && len(excludedDirs) > 0 {
		uniq := make([]string, 0, len(excludedDirs))
		for k := range excludedDirs {
			uniq = append(uniq, k)
		}
		sort.Strings(uniq)
		lines = append(lines, "Excluded: "+strings.Join(uniq, " "))
	}
	return strings.Join(lines, "\n")
}

func fmtSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	} else if n < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}

func toolSearchFiles(workspace string, inp map[string]any) string {
	pattern, _ := inp["pattern"].(string)
	if pattern == "" {
		return "Error: 'pattern' is required for search_files."
	}
	searchPath, _ := inp["path"].(string)
	if searchPath == "" {
		searchPath = "."
	}
	fileGlob, _ := inp["file_pattern"].(string)
	if fileGlob == "" {
		fileGlob = "*"
	}
	caseSensitive := true
	if v, ok := inp["case_sensitive"].(bool); ok {
		caseSensitive = v
	}
	maxResults := 50
	if v, ok := inp["max_results"].(float64); ok {
		maxResults = int(v)
	}
	if maxResults < 1 {
		maxResults = 1
	} else if maxResults > 200 {
		maxResults = 200
	}

	base, err := safeJoin(workspace, searchPath)
	if err != nil {
		return "Error: " + err.Error()
	}

	var re *regexp.Regexp
	if caseSensitive {
		re, err = regexp.Compile(pattern)
	} else {
		re, err = regexp.Compile("(?i)" + pattern)
	}
	if err != nil {
		return "Error: invalid regex pattern — " + err.Error()
	}

	type hit struct {
		lineno int
		line   string
	}
	type fileHits struct {
		rel  string
		hits []hit
	}
	var results []fileHits
	totalMatches := 0

	err = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if skipFiles[d.Name()] {
			return nil
		}
		if fileGlob != "*" {
			matched, _ := filepath.Match(fileGlob, d.Name())
			if !matched {
				return nil
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		// Skip binary files
		if !isText(data) {
			return nil
		}
		rel, _ := filepath.Rel(workspace, path)
		var fileResult []hit
		for lineno, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				fileResult = append(fileResult, hit{lineno: lineno + 1, line: strings.TrimRight(line, "\r")})
				totalMatches++
				if totalMatches >= maxResults {
					break
				}
			}
		}
		if len(fileResult) > 0 {
			results = append(results, fileHits{rel: rel, hits: fileResult})
		}
		if totalMatches >= maxResults {
			return filepath.SkipAll
		}
		return nil
	})
	_ = err

	if len(results) == 0 {
		return fmt.Sprintf("No matches found for '%s'.", pattern)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Search results for '%s':", pattern))
	for _, r := range results {
		lines = append(lines, "\n"+r.rel+":")
		for _, h := range r.hits {
			lines = append(lines, fmt.Sprintf("  %d: %s", h.lineno, h.line))
		}
	}
	if totalMatches >= maxResults {
		lines = append(lines, fmt.Sprintf("\n(Showing first %d matches — narrow the search with a more specific pattern or path)", maxResults))
	}
	return strings.Join(lines, "\n")
}

// isText returns true if data looks like UTF-8 text (no null bytes in first 512 bytes).
func isText(data []byte) bool {
	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}
	for _, b := range sample {
		if b == 0 {
			return false
		}
	}
	return true
}
