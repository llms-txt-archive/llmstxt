package fetch

import "github.com/f-pisani/llmstxt/internal/fileutil"

// SafeJoin joins root and relativePath, returning an error if the result escapes root.
func SafeJoin(root string, relativePath string) (string, error) {
	return fileutil.SafeJoin(root, relativePath)
}
