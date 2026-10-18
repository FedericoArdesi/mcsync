// Package atomicfile scrive file in modo atomico: chi legge, o chi si riavvia
// dopo un guasto, trova il file vecchio o quello nuovo, mai una via di mezzo.
package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Write scrive data in un file temporaneo accanto a quello vero (path + ".tmp"),
// lo forza su disco e lo rinomina sopra al vecchio. Crea la cartella se manca.
// Va chiamata da un solo scrittore alla volta per lo stesso path.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := renameReplace(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(dir)
	return nil
}

// renameReplace rinomina sopra un file esistente. Su Windows go usa
// MoveFileEx con sostituzione, ma un antivirus che ha il file aperto può far
// fallire il tentativo per un attimo: si riprova qualche volta.
func renameReplace(from, to string) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		if runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}

// syncDir forza su disco la rinomina (serve su Linux, dove gira il servizio).
// È un passo in più "best effort": alcuni filesystem non lo supportano e su
// Windows non si può, e il file è comunque già al sicuro.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
