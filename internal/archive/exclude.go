package archive

import "strings"

// Excluded dice se un percorso relativo (con "/" come separatore) resta fuori
// dall'archivio (sezione 5.3): session.lock a qualunque profondità, e le
// cartelle logs, crash-reports e cache al livello più alto del mondo.
func Excluded(rel string) bool {
	parts := strings.Split(rel, "/")
	switch parts[0] {
	case "logs", "crash-reports", "cache":
		return true
	}
	return parts[len(parts)-1] == "session.lock"
}
