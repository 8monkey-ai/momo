// Package agent runs a conversation's turn on an agent harness.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// unsafe is every character a directory name must not carry. Replacing all of
// them keeps the name one segment, and never "." or "..".
var unsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// dirName turns a conversation identity into one directory name: a readable
// part for the operator who looks at the data root, and a digest of the whole
// identity so that two identities the readable part cannot tell apart still get
// their own directory.
func dirName(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	readable := unsafe.ReplaceAllString(identity, "-")
	if len(readable) > 64 {
		readable = readable[:64]
	}
	return readable + "-" + hex.EncodeToString(sum[:8])
}
