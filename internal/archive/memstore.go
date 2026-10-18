package archive

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type blockHash = [sha256.Size]byte

// MemStore è un archivio finto: tiene i dati in memoria, ma legge e scrive
// cartelle vere come farà l'implementazione con restic. Deduplica per file
// intero (l'archivio vero lo fa per blocchi): basta per provare il protocollo.
type MemStore struct {
	mu     sync.Mutex
	seq    int
	blocks map[blockHash][]byte
	snaps  map[SnapshotID]map[string]blockHash

	// BeforeFile, se non è nil, viene chiamata prima di leggere ogni file
	// durante Put. Se restituisce un errore, Put si interrompe: serve a
	// simulare una caduta di rete a metà upload. Va impostata prima di Put.
	BeforeFile func(rel string) error
}

var _ Store = (*MemStore)(nil)

// NewMemStore crea un archivio finto vuoto.
func NewMemStore() *MemStore {
	return &MemStore{
		blocks: map[blockHash][]byte{},
		snaps:  map[SnapshotID]map[string]blockHash{},
	}
}

// Put salva il contenuto di src. È tutto o niente: la copia compare
// nell'archivio solo alla fine, se tutti i file sono stati letti.
func (m *MemStore) Put(ctx context.Context, src string) (PutResult, error) {
	files := map[string]blockHash{}
	pending := map[blockHash][]byte{}
	var total int64

	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if Excluded(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.BeforeFile != nil {
			if err := m.BeforeFile(rel); err != nil {
				return err
			}
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files[rel] = sum
		pending[sum] = data
		total += int64(len(data))
		return nil
	})
	if err != nil {
		return PutResult{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	var added int64
	for sum, data := range pending {
		if _, ok := m.blocks[sum]; !ok {
			m.blocks[sum] = data
			added += int64(len(data))
		}
	}
	m.seq++
	id := SnapshotID(fmt.Sprintf("snap-%04d", m.seq))
	m.snaps[id] = files
	return PutResult{ID: id, AddedBytes: added, TotalBytes: total, Files: len(files)}, nil
}

// Get ripristina la copia in dst, che non deve esistere. I file vengono
// scritti in una cartella provvisoria e spostati in dst solo alla fine.
func (m *MemStore) Get(ctx context.Context, id SnapshotID, dst string) error {
	m.mu.Lock()
	if err := m.verifyLocked(id); err != nil {
		m.mu.Unlock()
		return err
	}
	out := make(map[string][]byte, len(m.snaps[id]))
	for rel, sum := range m.snaps[id] {
		out[rel] = m.blocks[sum]
	}
	m.mu.Unlock()

	if _, err := os.Stat(dst); err == nil {
		return ErrDstExists
	}
	tmp := dst + ".partial"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return fail(err)
	}
	for rel, data := range out {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		full := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fail(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return fail(err)
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fail(err)
	}
	return nil
}

// Verify ricalcola gli hash di tutti i blocchi della copia.
func (m *MemStore) Verify(ctx context.Context, id SnapshotID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.verifyLocked(id)
}

func (m *MemStore) verifyLocked(id SnapshotID) error {
	files, ok := m.snaps[id]
	if !ok {
		return ErrNotFound
	}
	for rel, sum := range files {
		data, ok := m.blocks[sum]
		if !ok || sha256.Sum256(data) != sum {
			return fmt.Errorf("%w: %s", ErrCorrupt, rel)
		}
	}
	return nil
}

// Delete toglie una copia dall'elenco. I blocchi restano (la pulizia dei
// blocchi non più usati arriverà con la retention).
func (m *MemStore) Delete(ctx context.Context, id SnapshotID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.snaps[id]; !ok {
		return ErrNotFound
	}
	delete(m.snaps, id)
	return nil
}

// List restituisce gli identificativi delle copie, in ordine.
func (m *MemStore) List(ctx context.Context) ([]SnapshotID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]SnapshotID, 0, len(m.snaps))
	for id := range m.snaps {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// Corrupt rovina di proposito il blocco di un file di una copia, per i test.
// Se lo stesso contenuto è condiviso con altre copie, si rovinano anche
// quelle, come succederebbe in un archivio vero.
func (m *MemStore) Corrupt(id SnapshotID, rel string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	files, ok := m.snaps[id]
	if !ok {
		return ErrNotFound
	}
	sum, ok := files[rel]
	if !ok {
		return fmt.Errorf("file %q assente nella copia %s", rel, id)
	}
	data := m.blocks[sum]
	if len(data) == 0 {
		m.blocks[sum] = []byte{0}
		return nil
	}
	data[0] ^= 0xFF
	return nil
}
