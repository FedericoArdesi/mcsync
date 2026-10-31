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

	"github.com/FedericoArdesi/mcsync/internal/atomicfile"
	"github.com/FedericoArdesi/mcsync/internal/protocol"
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

// Save scrive lo stato in modo atomico (vedi atomicfile.Write).
func (s *Store) Save(st protocol.State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path, append(data, '\n'), 0o600)
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
