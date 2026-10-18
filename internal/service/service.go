// Package service collega il coordinatore del protocollo all'archivio: è il
// cuore logico del servizio sul laptop, ancora senza rete né disco vero.
package service

import (
	"context"
	"errors"

	"mcsync/internal/archive"
	"mcsync/internal/protocol"
)

var ErrUnknownVersion = errors.New("versione sconosciuta")

// Service unisce le regole del protocollo e l'archivio delle versioni.
type Service struct {
	coord *protocol.Coordinator
	store archive.Store
}

// New crea un servizio.
func New(coord *protocol.Coordinator, store archive.Store) *Service {
	return &Service{coord: coord, store: store}
}

// Upload salva la cartella src nell'archivio e poi chiede il commit.
//
// Se il salvataggio fallisce a metà non resta niente e il puntatore non si
// muove. Se il salvataggio riesce ma il commit viene rifiutato (generazione
// vecchia, base non più corrente, spazio), la copia resta nell'archivio e il
// coordinatore la registra tra gli orfani con il suo identificativo: il lavoro
// non si perde (6.2).
func (s *Service) Upload(ctx context.Context, player, device string, gen, parent uint64, src string) (protocol.Version, archive.PutResult, error) {
	res, err := s.store.Put(ctx, src)
	if err != nil {
		return protocol.Version{}, archive.PutResult{}, err
	}
	v, err := s.coord.Commit(protocol.CommitRequest{
		Player:         player,
		Device:         device,
		Generation:     gen,
		Parent:         parent,
		BlocksVerified: true, // Put restituisce solo copie complete e verificate
		SizeBytes:      res.AddedBytes,
		Snapshot:       string(res.ID),
	})
	if err != nil {
		return protocol.Version{}, res, err
	}
	return v, res, nil
}

// Download ripristina in dst il contenuto di una versione ufficiale.
func (s *Service) Download(ctx context.Context, version uint64, dst string) error {
	v, ok := s.coord.Version(version)
	if !ok {
		return ErrUnknownVersion
	}
	return s.store.Get(ctx, archive.SnapshotID(v.Snapshot), dst)
}

// DownloadCurrent ripristina in dst la versione ufficiale corrente e dice
// quale numero era.
func (s *Service) DownloadCurrent(ctx context.Context, dst string) (uint64, error) {
	n := s.coord.Pointer()
	return n, s.Download(ctx, n, dst)
}
