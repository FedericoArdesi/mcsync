// Package archive definisce il contratto dell'archivio delle versioni del
// mondo (sezione 5.1 del documento di progetto). Il protocollo non sa come
// sono salvati i dati: parla solo con Store. Così si può testare tutto con un
// archivio finto in memoria (MemStore) e aggiungere dopo l'implementazione con
// restic dietro lo stesso contratto.
package archive

import (
	"context"
	"errors"
)

var (
	ErrNotFound  = errors.New("snapshot non trovata")
	ErrCorrupt   = errors.New("blocco corrotto: l'hash non corrisponde")
	ErrDstExists = errors.New("la destinazione esiste già")
)

// SnapshotID identifica una copia del mondo nell'archivio.
type SnapshotID string

// PutResult descrive una copia appena salvata.
type PutResult struct {
	ID SnapshotID
	// AddedBytes sono i byte nuovi aggiunti all'archivio (quelli che
	// viaggiano in rete e occupano spazio).
	AddedBytes int64
	// TotalBytes è la dimensione totale del mondo salvato.
	TotalBytes int64
	Files      int
}

// Store è l'archivio delle versioni. Regole del contratto:
//
//   - Put è tutto o niente: se restituisce un errore, nell'archivio non resta
//     nessuna copia a metà (principio 2). Se ha successo, i blocchi sono
//     arrivati per intero e gli hash sono stati controllati.
//   - Put ignora i file esclusi (vedi Excluded).
//   - Get ripristina in una cartella che non deve esistere e che compare solo
//     a ripristino completato e verificato.
//   - Nessuna operazione modifica una copia già salvata.
type Store interface {
	Put(ctx context.Context, src string) (PutResult, error)
	Get(ctx context.Context, id SnapshotID, dst string) error
	Verify(ctx context.Context, id SnapshotID) error
	Delete(ctx context.Context, id SnapshotID) error
	List(ctx context.Context) ([]SnapshotID, error)
}
