package archive_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/FedericoArdesi/mcsync/internal/archive"
	"github.com/FedericoArdesi/mcsync/internal/archive/storetest"
)

// TestResticStoreContract usa il restic installato sul PC. Se non lo trova nel
// PATH il test viene saltato; per indicare un altro percorso si imposta la
// variabile d'ambiente MCSYNC_RESTIC.
func TestResticStoreContract(t *testing.T) {
	bin := os.Getenv("MCSYNC_RESTIC")
	if bin == "" {
		p, err := exec.LookPath("restic")
		if err != nil {
			t.Skip("restic non trovato nel PATH (imposta MCSYNC_RESTIC con il percorso di restic.exe)")
		}
		bin = p
	}
	storetest.Run(t, func(t *testing.T) archive.Store {
		s := archive.NewResticStore(bin, filepath.Join(t.TempDir(), "repo"), "prova")
		if err := s.EnsureRepo(context.Background()); err != nil {
			t.Fatalf("creazione del repository di prova: %v", err)
		}
		return s
	})
}
