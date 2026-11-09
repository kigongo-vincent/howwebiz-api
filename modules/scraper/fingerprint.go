package scraper

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Fingerprint returns a stable hash of parsed data. howwe.ug serves
// different HTML on every request (no ETag/Last-Modified, rotating tokens),
// so change detection is done on the parsed result instead of the bytes.
func Fingerprint(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}
