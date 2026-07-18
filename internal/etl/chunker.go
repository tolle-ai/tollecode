package etl

const (
	chunkSize    = 1800 // runes per chunk
	chunkOverlap = 200  // runes of overlap between adjacent chunks
)

// Chunk splits text into overlapping windows suitable for embedding.
func Chunk(text string) []string {
	if len(text) == 0 {
		return nil
	}
	runes := []rune(text)
	total := len(runes)
	var chunks []string
	start := 0
	for start < total {
		end := start + chunkSize
		if end > total {
			end = total
		}
		chunks = append(chunks, string(runes[start:end]))
		if end == total {
			break
		}
		start += chunkSize - chunkOverlap
	}
	return chunks
}
