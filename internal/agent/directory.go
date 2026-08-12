package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// directoryName turns a conversation identity into one safe directory name, so
// no channel has to be trusted to supply one. The hash keeps identities that
// differ only in unsafe characters apart.
func directoryName(conversation string) string {
	var safe strings.Builder
	for _, r := range conversation {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			safe.WriteRune(r)
		default:
			safe.WriteByte('_')
		}
	}
	sum := sha256.Sum256([]byte(conversation))
	name := safe.String()
	if len(name) > 64 {
		name = name[:64]
	}
	return name + "-" + hex.EncodeToString(sum[:4])
}
