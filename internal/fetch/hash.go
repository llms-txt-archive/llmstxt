package fetch

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashBytes returns the hex-encoded SHA-256 digest of body.
func HashBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
