// Package statefile salva su disco lo stato del coordinatore del protocollo,
// con scrittura atomica, e lo ricarica al riavvio del servizio.
//
// Un solo file JSON contiene lease, puntatore, versioni e orfani: così si
// aggiornano sempre insieme, e dopo un blackout si trova o lo stato vecchio o
// quello nuovo, mai una via di mezzo.
package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"mcsync/internal/protocol"
)

// Store salva e carica lo stato in un file. Implementa protocol.Saver.
type Store struct {
	path string
}

var _ protocol.Saver = (*Store)(nil)

// New crea uno Store sul file indicato (di solito state/state.json). Il file
// e la cartella vengono creati al primo salvataggio.
func New(path string) *Store { return &Store{path: path} }

// Path restituisce il percorso del file di stato.
func (s *Store) Path() string { return s.path }

// Save scrive lo stato in un file temporaneo accanto a quello vero, lo forza
// su disco e poi lo rinomina sopra al vecchio. La rinomina è atomica: chi
// legge, o chi si riavvia dopo un guasto, vede il file vecchio o quello nuovo.
func (s *Store) Save(st protocol.State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
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
	if err := renameReplace(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(dir)
	return nil
}

// Load legge lo stato. Se il file non esiste restituisce found=false senza
// errore (primo avvio). Se esiste ma non si legge, è un errore: non si riparte
// da zero in silenzio, perché la generazione tornerebbe indietro.
func (s *Store) Load() (st protocol.State, found bool, err error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return protocol.State{}, false, nil
	}
	if err != nil {
		return protocol.State{}, false, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return protocol.State{}, false, fmt.Errorf("%w: %v", protocol.ErrBadState, err)
	}
	return st, true, nil
}

// Open crea il coordinatore del servizio: riparte dallo stato salvato in path,
// o da zero se è il primo avvio, e da lì in poi lo salva a ogni modifica.
func Open(path string, clock protocol.Clock, freeBytes func() int64) (*protocol.Coordinator, error) {
	s := New(path)
	st, found, err := s.Load()
	if err != nil {
		return nil, fmt.Errorf("stato in %s: %w", path, err)
	}
	if !found {
		return protocol.NewPersistent(clock, freeBytes, s, nil)
	}
	c, err := protocol.NewPersistent(clock, freeBytes, s, &st)
	if err != nil {
		return nil, fmt.Errorf("stato in %s: %w", path, err)
	}
	return c, nil
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
// Windows non si può, e il file di stato è comunque già al sicuro.
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
