package archive

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ResticStore implementa Store chiamando restic come programma esterno.
//
// La password del repository viaggia solo nell'ambiente del processo
// (RESTIC_PASSWORD), mai negli argomenti, così non compare nell'elenco dei
// processi (sezione 8.2: nessun segreto nei posti sbagliati).
//
// Limiti noti, da verificare con i test del contratto:
//   - Verify esegue "restic check" (struttura del repository), non rilegge
//     tutti i dati; Get ripristina con --verify, quindi un blocco rovinato
//     emerge comunque al ripristino, ma come errore generico, non ErrCorrupt.
//   - Se restic termina con un errore dopo aver creato la copia (per esempio
//     alcuni file illeggibili), Put restituisce un errore e la copia resta
//     nel repository come snapshot non usata.
//   - I percorsi di esclusione sono passati come pattern di restic: un
//     percorso del mondo con caratteri come "[" potrebbe essere letto come
//     pattern.
type ResticStore struct {
	Binary   string // percorso di restic (o solo "restic" se è nel PATH)
	Repo     string // cartella del repository
	Password string
}

var _ Store = (*ResticStore)(nil)

// NewResticStore crea uno Store che usa restic. Il repository va creato una
// volta con EnsureRepo.
func NewResticStore(binary, repo, password string) *ResticStore {
	return &ResticStore{Binary: binary, Repo: repo, Password: password}
}

type snapshotInfo struct {
	ID      string   `json:"id"`
	ShortID string   `json:"short_id"`
	Paths   []string `json:"paths"`
}

type backupSummary struct {
	MessageType     string `json:"message_type"`
	SnapshotID      string `json:"snapshot_id"`
	DataAdded       int64  `json:"data_added"`
	DataAddedPacked int64  `json:"data_added_packed"`
	TotalFiles      int    `json:"total_files_processed"`
	TotalBytes      int64  `json:"total_bytes_processed"`
}

// run esegue restic con gli argomenti dati e restituisce lo standard output.
func (s *ResticStore) run(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"-r", s.Repo}, args...)
	cmd := exec.CommandContext(ctx, s.Binary, full...)
	cmd.Env = append(os.Environ(), "RESTIC_PASSWORD="+s.Password)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("restic %s: %w", args[0], ctx.Err())
		}
		return out.Bytes(), fmt.Errorf("restic %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// EnsureRepo crea il repository se non esiste ancora.
func (s *ResticStore) EnsureRepo(ctx context.Context) error {
	if _, err := s.run(ctx, "cat", "config"); err == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.run(ctx, "init")
	return err
}

// Put salva src con "restic backup". Restic scrive la copia solo alla fine,
// quindi un'interruzione non lascia niente a metà.
func (s *ResticStore) Put(ctx context.Context, src string) (PutResult, error) {
	abs, err := filepath.Abs(src)
	if err != nil {
		return PutResult{}, err
	}
	// Stesse esclusioni di Excluded: session.lock a qualunque profondità e
	// tre cartelle al livello più alto del mondo.
	args := []string{"backup", abs, "--json", "--host", "mcsync", "--exclude", "session.lock"}
	for _, d := range []string{"logs", "crash-reports", "cache"} {
		args = append(args, "--exclude", filepath.Join(abs, d))
	}
	out, err := s.run(ctx, args...)
	if err != nil {
		return PutResult{}, err
	}
	sum, err := parseSummary(out)
	if err != nil {
		return PutResult{}, err
	}
	info, err := s.lookup(ctx, sum.SnapshotID)
	if err != nil {
		return PutResult{}, fmt.Errorf("restic: snapshot appena creata non trovata: %w", err)
	}
	added := sum.DataAddedPacked
	if added == 0 {
		added = sum.DataAdded
	}
	return PutResult{
		ID:         SnapshotID(info.ID),
		AddedBytes: added,
		TotalBytes: sum.TotalBytes,
		Files:      sum.TotalFiles,
	}, nil
}

// Get ripristina la copia in dst, che non deve esistere.
//
// Si ripristina solo la cartella del mondo ("snapshot:sottocartella"), non
// l'intero percorso originale: altrimenti restic ricrea anche le cartelle
// sopra il mondo (su Windows C:\Users) e tenta di reimpostarne data e
// attributi, ottenendo "Accesso negato". I file vengono scritti in una
// cartella provvisoria e spostati in dst solo a ripristino completato.
func (s *ResticStore) Get(ctx context.Context, id SnapshotID, dst string) error {
	info, err := s.lookup(ctx, string(id))
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return ErrDstExists
	}
	if len(info.Paths) == 0 {
		return fmt.Errorf("restic: la snapshot %s non ha percorsi", info.ID)
	}
	tmp := dst + ".partial"
	fail := func(err error) error {
		_ = os.RemoveAll(tmp)
		return err
	}
	var firstErr error
	restored := false
	for _, sub := range snapshotSubfolders(info.Paths[0]) {
		if err := os.RemoveAll(tmp); err != nil {
			return err
		}
		_, err := s.run(ctx, "restore", info.ID+":"+sub, "--target", tmp, "--verify")
		if err == nil {
			restored = true
			break
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			return fail(err)
		}
	}
	if !restored {
		return fail(firstErr)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fail(err)
	}
	return nil
}

// Verify controlla che la copia esista e che il repository sia coerente.
func (s *ResticStore) Verify(ctx context.Context, id SnapshotID) error {
	if _, err := s.lookup(ctx, string(id)); err != nil {
		return err
	}
	_, err := s.run(ctx, "check")
	return err
}

// Delete toglie la copia dall'elenco con "restic forget" (senza prune: la
// pulizia dei blocchi inutilizzati arriverà con la retention).
func (s *ResticStore) Delete(ctx context.Context, id SnapshotID) error {
	info, err := s.lookup(ctx, string(id))
	if err != nil {
		return err
	}
	_, err = s.run(ctx, "forget", info.ID)
	return err
}

// List restituisce gli identificativi (completi) delle copie nel repository.
func (s *ResticStore) List(ctx context.Context) ([]SnapshotID, error) {
	infos, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]SnapshotID, 0, len(infos))
	for _, in := range infos {
		ids = append(ids, SnapshotID(in.ID))
	}
	return ids, nil
}

func (s *ResticStore) list(ctx context.Context) ([]snapshotInfo, error) {
	out, err := s.run(ctx, "snapshots", "--json")
	if err != nil {
		return nil, err
	}
	var infos []snapshotInfo
	if err := json.Unmarshal(out, &infos); err != nil {
		return nil, fmt.Errorf("restic snapshots: output non valido: %w", err)
	}
	return infos, nil
}

// lookup trova una snapshot per identificativo completo o per prefisso
// univoco di almeno 8 caratteri.
func (s *ResticStore) lookup(ctx context.Context, id string) (snapshotInfo, error) {
	if id == "" {
		return snapshotInfo{}, ErrNotFound
	}
	infos, err := s.list(ctx)
	if err != nil {
		return snapshotInfo{}, err
	}
	var found []snapshotInfo
	for _, in := range infos {
		if in.ID == id {
			return in, nil
		}
		if len(id) >= 8 && strings.HasPrefix(in.ID, id) {
			found = append(found, in)
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	return snapshotInfo{}, ErrNotFound
}

func parseSummary(out []byte) (backupSummary, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var m backupSummary
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if m.MessageType == "summary" && m.SnapshotID != "" {
			return m, nil
		}
	}
	return backupSummary{}, errors.New("restic: riepilogo del backup non trovato nell'output")
}

// snapshotSubfolders elenca i modi in cui il percorso originale può comparire
// dentro la snapshot di restic. Su Windows C:\a\b è registrato come "/C/a/b".
func snapshotSubfolders(original string) []string {
	vol := filepath.VolumeName(original)
	rest := filepath.ToSlash(original[len(vol):])
	if vol == "" {
		return []string{rest}
	}
	return []string{
		"/" + strings.TrimRight(vol, ":") + rest,
		filepath.ToSlash(original),
	}
}
