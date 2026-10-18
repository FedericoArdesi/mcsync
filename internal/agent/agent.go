// Package agent è la logica dell'agente sul PC del giocatore (4.3 del
// progetto): prende la lease, mette il mondo in locale all'ultima versione,
// avvia il gioco, tiene vivo il lock, manda il mondo al servizio ogni tot
// minuti e alla chiusura lo manda un'ultima volta prima di rilasciare la lease.
//
// Il gioco sta dietro l'interfaccia Game: oggi c'è solo FakeGame, il server
// Minecraft vero arriverà dopo senza toccare il resto.
//
// Regole che questo file fa rispettare:
//   - la lease si rilascia solo dopo la conferma del commit finale (6.6);
//   - un mondo locale non ancora confermato non si cancella mai: se deve
//     lasciare il posto a una versione più recente va in recovery (6.2);
//   - un host senza notizie del laptop si ferma da solo (6.5), prima che la
//     lease scada e un altro diventi host;
//   - un upload senza risposta si riconcilia guardando lo stato (7).
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"mcsync/internal/api"
	"mcsync/internal/archive"
	"mcsync/internal/atomicfile"
	"mcsync/internal/client"
	"mcsync/internal/tarutil"
)

// Remote è il servizio visto dall'agente. *client.Client lo implementa; i test
// lo avvolgono per far fallire la rete a comando.
type Remote interface {
	Status(ctx context.Context) (api.Status, error)
	Acquire(ctx context.Context) (api.Lease, error)
	Heartbeat(ctx context.Context, generation uint64) (api.Lease, error)
	Release(ctx context.Context, generation uint64, handoffTo string) error
	Upload(ctx context.Context, generation, parent uint64, dir string, skip func(rel string) bool) (api.UploadResponse, error)
	Download(ctx context.Context, version uint64, dst string, lim tarutil.Limits) (uint64, error)
}

var _ Remote = (*client.Client)(nil)

// Game è il server Minecraft (o il suo sostituto) controllato dall'agente.
type Game interface {
	// Start avvia il gioco sul mondo in world.
	Start(ctx context.Context, world string) error
	// Freeze fa scrivere tutto su disco e sospende i salvataggi automatici
	// (save-off, save-all flush); thaw li riattiva (save-on). Il gioco non si
	// ferma: i giocatori non se ne accorgono.
	Freeze(ctx context.Context) (thaw func(), err error)
	// Stop scrive tutto e ferma il gioco.
	Stop(ctx context.Context) error
	// Done si chiude quando il gioco è finito (da solo o dopo Stop).
	Done() <-chan struct{}
}

// Errori con cui Run si ferma senza aver chiuso la sessione in modo normale.
var (
	// ErrLeaseLost: il servizio dice che non siamo più host.
	ErrLeaseLost = errors.New("la lease non è più tua")
	// ErrIsolated: troppo tempo senza notizie del laptop (auto-blocco).
	ErrIsolated = errors.New("laptop irraggiungibile da troppo tempo")
	// ErrNotSaved: il salvataggio finale non è riuscito; la lease non è stata
	// rilasciata e il mondo locale resta segnato come non confermato.
	ErrNotSaved = errors.New("salvataggio finale non riuscito")
)

// LeaseHeldError: un altro giocatore è già host (o sta per diventarlo).
type LeaseHeldError struct {
	Holder    string // giocatore che ha la lease, o a cui è riservata
	Reserved  bool
	InSeconds int
}

func (e *LeaseHeldError) Error() string {
	if e.Reserved {
		return fmt.Sprintf("la lease è riservata a %s per un passaggio di host (ancora %d s)", e.Holder, e.InSeconds)
	}
	return fmt.Sprintf("%s sta già giocando", e.Holder)
}

// Options configura una sessione.
type Options struct {
	World string // cartella del mondo sul PC
	// WorkDir contiene stato, mirror, download in corso e recovery. Di default
	// World + ".mcsync" (accanto al mondo, sullo stesso disco).
	WorkDir string
	Game    Game
	Log     *slog.Logger

	UploadEvery    time.Duration // default 10 minuti (6.4)
	HeartbeatEvery time.Duration // default 1 minuto (6.1)
	WarnAfter      time.Duration // senza laptop: avviso, default 3 minuti (6.5)
	StopAfter      time.Duration // senza laptop: stop del gioco, default 8 minuti
	CallTimeout    time.Duration // richieste brevi, default 15 secondi

	// Salvataggio finale: tentativi e attesa tra l'uno e l'altro.
	FinalAttempts int           // default 3
	FinalBackoff  time.Duration // default 5 secondi, raddoppia
	StopTimeout   time.Duration // attesa dello stop del gioco, default 2 minuti
}

func (o *Options) defaults() {
	if o.WorkDir == "" {
		o.WorkDir = o.World + ".mcsync"
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	setDur := func(p *time.Duration, d time.Duration) {
		if *p <= 0 {
			*p = d
		}
	}
	setDur(&o.UploadEvery, 10*time.Minute)
	setDur(&o.HeartbeatEvery, time.Minute)
	setDur(&o.WarnAfter, 3*time.Minute)
	setDur(&o.StopAfter, 8*time.Minute)
	setDur(&o.CallTimeout, 15*time.Second)
	setDur(&o.FinalBackoff, 5*time.Second)
	setDur(&o.StopTimeout, 2*time.Minute)
	if o.FinalAttempts <= 0 {
		o.FinalAttempts = 3
	}
}

// LocalState è quello che l'agente ricorda del mondo su questo PC (4.3).
type LocalState struct {
	// BaseVersion è la versione ufficiale da cui discende la copia locale.
	BaseVersion uint64 `json:"base_version"`
	// Dirty: la copia locale può avere modifiche non ancora confermate dal
	// servizio. Si accende prima di avviare il gioco e si spegne solo dopo il
	// commit finale.
	Dirty bool `json:"dirty"`
}

// ReadState legge lo stato locale (vuoto se non esiste ancora).
func ReadState(workDir string) (LocalState, error) {
	var st LocalState
	b, err := os.ReadFile(statePath(workDir))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("stato locale rovinato (%s): %w", statePath(workDir), err)
	}
	return st, nil
}

func statePath(workDir string) string { return filepath.Join(workDir, "state.json") }

type session struct {
	r   Remote
	o   Options
	log *slog.Logger

	player, device string
	gen            uint64
	st             LocalState
	lastFP         string // impronta del mondo all'ultimo stato confermato dal servizio

	fatalCh chan error
}

// Run fa una sessione di gioco completa e torna quando è finita: Ctrl+C
// (ctx annullato), il gioco che termina, o un guasto che impone di fermarsi.
// Restituisce nil solo se il mondo è stato salvato sul servizio e la lease
// rilasciata.
func Run(ctx context.Context, r Remote, o Options) error {
	o.defaults()
	if o.World == "" || o.Game == nil {
		return errors.New("agent: servono World e Game")
	}
	world, err := filepath.Abs(o.World)
	if err != nil {
		return err
	}
	if filepath.Dir(world) == world {
		return errors.New("agent: il mondo non può essere la radice di un disco")
	}
	o.World = world
	if err := os.MkdirAll(o.WorkDir, 0o755); err != nil {
		return err
	}
	s := &session{r: r, o: o, log: o.Log, fatalCh: make(chan error, 1)}

	if s.st, err = ReadState(o.WorkDir); err != nil {
		return err
	}

	// 1. il laptop deve rispondere (la modalità emergenza non c'è ancora)
	st, err := s.status(ctx)
	if err != nil {
		return fmt.Errorf("il laptop non risponde (la modalità emergenza non è ancora disponibile): %w", err)
	}
	s.player, s.device = st.You.Player, st.You.Device

	// 2. lease
	lease, err := s.acquire(ctx, st)
	if err != nil {
		return err
	}
	s.gen = lease.Generation
	s.log.Info("sei host", "giocatore", s.player, "generazione", s.gen)

	// 3. il mondo locale all'ultima versione. Dopo la presa della lease, perché
	// solo chi ha la lease può far cambiare il puntatore.
	if err := s.syncWorld(ctx); err != nil {
		s.releaseQuietly(ctx)
		return err
	}

	// 4. gioco
	s.st.Dirty = true
	if err := s.saveState(); err != nil {
		s.releaseQuietly(ctx)
		return err
	}
	if err := o.Game.Start(ctx, s.o.World); err != nil {
		// il gioco non è partito: nessuna modifica, il mondo resta come era
		s.log.Error("il gioco non è partito", "errore", err)
		if rerr := s.stopGame(ctx); rerr != nil {
			s.log.Warn("stop del gioco", "errore", rerr)
		}
		return s.finish(ctx, nil)
	}
	s.log.Info("gioco avviato: puoi giocare")

	hbCtx, stopHB := context.WithCancel(context.WithoutCancel(ctx))
	var hbDone sync.WaitGroup
	hbDone.Add(1)
	go func() { defer hbDone.Done(); s.heartbeatLoop(hbCtx) }()
	defer func() { stopHB(); hbDone.Wait() }()

	// 5. partita
	reason := s.playLoop(ctx)
	return s.finish(ctx, reason)
}

// playLoop gira finché non c'è un motivo per fermarsi. Restituisce nil per una
// chiusura normale, un errore per un guasto.
func (s *session) playLoop(ctx context.Context) error {
	t := time.NewTicker(s.o.UploadEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.log.Info("chiusura richiesta")
			return nil
		case <-s.o.Game.Done():
			s.log.Info("il gioco è terminato")
			return nil
		case err := <-s.fatalCh:
			return err
		case <-t.C:
			if err := s.uploadCycle(ctx); err != nil {
				if isDefinitive(err) {
					return s.lostErr(err)
				}
				if ctx.Err() == nil {
					s.log.Warn("upload non riuscito, riprovo al prossimo giro", "errore", err)
				}
			}
		}
	}
}

// finish chiude la sessione. reason è nil per la chiusura normale.
func (s *session) finish(ctx context.Context, reason error) error {
	// fermare il gioco non deve dipendere dal Ctrl+C che ci ha portati qui
	if err := s.stopGame(ctx); err != nil {
		s.log.Warn("lo stop del gioco non è andato a buon fine", "errore", err)
	}
	switch {
	case errors.Is(reason, ErrLeaseLost):
		s.log.Error("non sei più host: il gioco è stato fermato. La copia locale resta sul PC e non è stata caricata", "errore", reason)
		return reason
	case errors.Is(reason, ErrIsolated):
		s.log.Error("gioco fermato per mancanza del laptop. La copia locale resta sul PC; la lease scadrà da sola", "errore", reason)
		return reason
	case reason != nil:
		return reason
	}

	if err := s.finalUpload(context.WithoutCancel(ctx)); err != nil {
		s.log.Error("il mondo NON è stato salvato sul laptop: non spegnere il PC finché non si risolve; la lease resta tua finché scade", "errore", err)
		return fmt.Errorf("%w: %w", ErrNotSaved, err)
	}
	s.st.Dirty = false
	if err := s.saveState(); err != nil {
		s.log.Warn("stato locale non salvato", "errore", err)
	}
	s.releaseQuietly(ctx)
	s.log.Info("mondo salvato e lease rilasciata: puoi spegnere il PC", "versione", s.st.BaseVersion)
	return nil
}

func (s *session) stopGame(ctx context.Context) error {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.o.StopTimeout)
	defer cancel()
	return s.o.Game.Stop(sctx)
}

func (s *session) releaseQuietly(ctx context.Context) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.o.CallTimeout)
	defer cancel()
	if err := s.r.Release(rctx, s.gen, ""); err != nil {
		s.log.Warn("lease non rilasciata (scadrà da sola)", "errore", err)
	}
}

// ---------------------------------------------------------------------------
// Avvio: lease e mondo locale

func (s *session) status(ctx context.Context) (api.Status, error) {
	cctx, cancel := context.WithTimeout(ctx, s.o.CallTimeout)
	defer cancel()
	return s.r.Status(cctx)
}

func (s *session) acquire(ctx context.Context, st api.Status) (api.Lease, error) {
	cctx, cancel := context.WithTimeout(ctx, s.o.CallTimeout)
	defer cancel()
	l, err := s.r.Acquire(cctx)
	if err == nil {
		return l, nil
	}
	switch {
	case client.IsCode(err, api.CodeLeaseHeld):
		e := &LeaseHeldError{}
		if now, serr := s.status(ctx); serr == nil && now.Lease != nil {
			e.Holder, e.InSeconds = now.Lease.Player, now.Lease.ExpiresInSeconds
		}
		return api.Lease{}, e
	case client.IsCode(err, api.CodeLeaseReserved):
		e := &LeaseHeldError{Reserved: true}
		if now, serr := s.status(ctx); serr == nil && now.Reservation != nil {
			e.Holder, e.InSeconds = now.Reservation.For, now.Reservation.ExpiresInSeconds
		}
		return api.Lease{}, e
	}
	return api.Lease{}, fmt.Errorf("presa della lease: %w", err)
}

func (s *session) syncWorld(ctx context.Context) error {
	st, err := s.status(ctx)
	if err != nil {
		return fmt.Errorf("stato del servizio: %w", err)
	}
	pointer := st.Pointer
	hasLocal := nonEmptyDir(s.o.World)

	switch {
	case pointer == 0:
		// nessuna versione ufficiale: si parte dal mondo locale (anche vuoto);
		// la prima versione nascerà dal primo upload
		s.log.Info("non esiste ancora una versione ufficiale: si parte dal mondo locale")
		if !hasLocal {
			s.st = LocalState{}
		}
		return nil
	case hasLocal && s.st.BaseVersion == pointer:
		if s.st.Dirty {
			// 4.3 prevede la verifica con checksum: non c'è ancora. La copia
			// locale si usa com'è, come da 7 ("rientra da solo").
			s.log.Warn("la copia locale era segnata come non confermata (chiusura brusca?): la uso senza verifica", "versione", pointer)
		} else {
			s.log.Info("la copia locale è già l'ultima versione, niente da scaricare", "versione", pointer)
			s.lastFP = fingerprint(s.o.World)
		}
		return nil
	}

	// scaricare: prima in una cartella a parte, poi si scambia
	incoming := filepath.Join(s.o.WorkDir, "incoming")
	if err := os.RemoveAll(incoming); err != nil {
		return err
	}
	s.log.Info("scarico la versione ufficiale", "versione", pointer)
	got, err := s.downloadTo(ctx, pointer, incoming)
	if err != nil {
		_ = os.RemoveAll(incoming)
		return fmt.Errorf("download: %w", err)
	}
	if hasLocal {
		// copia non confermata, o di cui non si sa niente: mai cancellarla
		if s.st.Dirty || s.st.BaseVersion == 0 {
			dst := filepath.Join(s.o.WorkDir, "recovery", time.Now().Format("20060102-150405"))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := renameRetry(s.o.World, dst); err != nil {
				_ = os.RemoveAll(incoming)
				return fmt.Errorf("spostamento della copia locale in recovery: %w", err)
			}
			s.log.Warn("la tua copia locale non era confermata: è in recovery", "cartella", dst)
		} else if err := os.RemoveAll(s.o.World); err != nil {
			_ = os.RemoveAll(incoming)
			return err
		}
	} else if err := os.RemoveAll(s.o.World); err != nil { // una cartella vuota nel mezzo
		_ = os.RemoveAll(incoming)
		return err
	}
	if err := renameRetry(incoming, s.o.World); err != nil {
		return fmt.Errorf("messa a posto del mondo scaricato: %w", err)
	}
	s.st = LocalState{BaseVersion: got}
	if err := s.saveState(); err != nil {
		return err
	}
	s.lastFP = fingerprint(s.o.World)
	s.log.Info("mondo aggiornato", "versione", got)
	return nil
}

func (s *session) downloadTo(ctx context.Context, version uint64, dst string) (uint64, error) {
	// il download non ha una scadenza propria: dipende dalla banda. Lo
	// ferma il Ctrl+C, o il servizio se il trasferimento resta fermo.
	return s.r.Download(ctx, version, dst, tarutil.Limits{MaxFiles: 4_000_000, MaxBytes: 256 << 30})
}

// ---------------------------------------------------------------------------
// Partita: heartbeat e upload

func (s *session) fatal(err error) {
	select {
	case s.fatalCh <- err:
	default:
	}
}

func (s *session) lostErr(err error) error {
	var ae *client.APIError
	if errors.As(err, &ae) && ae.Snapshot != "" {
		s.log.Warn("il servizio ha conservato comunque quello che hai caricato", "copia", ae.Snapshot)
	}
	return fmt.Errorf("%w: %w", ErrLeaseLost, err)
}

// isDefinitive dice se l'errore è una risposta del servizio che impone di
// fermarsi (non siamo host, il token non vale, il protocollo non combacia).
func isDefinitive(err error) bool {
	return client.IsCode(err,
		api.CodeLeaseExpired, api.CodeNoLease, api.CodeStaleGeneration,
		api.CodeLeaseHeld, api.CodeLeaseReserved, api.CodeParentMismatch,
		api.CodeUnauthorized, api.CodeProtocol)
}

func (s *session) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(s.o.HeartbeatEvery)
	defer t.Stop()
	lastOK := time.Now()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, s.o.CallTimeout)
		_, err := s.r.Heartbeat(cctx, s.gen)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			lastOK, warned = time.Now(), false
			continue
		}
		if isDefinitive(err) {
			s.fatal(s.lostErr(err))
			return
		}
		gap := time.Since(lastOK)
		s.log.Warn("il laptop non risponde", "da", gap.Round(time.Second), "errore", err)
		if gap >= s.o.StopAfter {
			s.fatal(fmt.Errorf("%w (%s)", ErrIsolated, gap.Round(time.Second)))
			return
		}
		if gap >= s.o.WarnAfter && !warned {
			warned = true
			s.log.Warn("attenzione: senza il laptop il mondo non viene salvato; se non torna presto il gioco si ferma da solo")
		}
	}
}

// uploadCycle manda il mondo al servizio mentre il gioco continua: freeze,
// copia nel mirror, thaw, upload dal mirror (6.4). Salta se non è cambiato
// niente dall'ultimo stato confermato.
func (s *session) uploadCycle(ctx context.Context) error {
	fp := fingerprint(s.o.World)
	if fp == s.lastFP {
		s.log.Info("nessuna modifica dall'ultimo salvataggio")
		return nil
	}
	mirror := filepath.Join(s.o.WorkDir, "mirror")
	thaw, err := s.o.Game.Freeze(ctx)
	if err != nil {
		return fmt.Errorf("freeze del gioco: %w", err)
	}
	err = refreshMirror(s.o.World, mirror)
	thaw()
	if err != nil {
		return fmt.Errorf("copia del mondo: %w", err)
	}
	return s.uploadFrom(ctx, mirror, fp)
}

// finalUpload: gioco già fermo, si carica direttamente dal mondo (nessuno lo
// modifica più), con qualche tentativo.
func (s *session) finalUpload(ctx context.Context) error {
	fp := fingerprint(s.o.World)
	if fp == s.lastFP {
		s.log.Info("nessuna modifica dall'ultimo salvataggio: niente da caricare")
		return nil
	}
	if !nonEmptyDir(s.o.World) {
		return errors.New("il mondo locale è vuoto: niente da caricare")
	}
	backoff := s.o.FinalBackoff
	var err error
	for i := 1; i <= s.o.FinalAttempts; i++ {
		s.log.Info("salvataggio finale", "tentativo", i)
		if err = s.uploadFrom(ctx, s.o.World, fp); err == nil {
			return nil
		}
		if isDefinitive(err) || i == s.o.FinalAttempts {
			return err
		}
		s.log.Warn("salvataggio finale non riuscito, riprovo", "errore", err, "tra", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return err
		}
		backoff *= 2
	}
	return err
}

func (s *session) uploadFrom(ctx context.Context, dir, fp string) error {
	start := time.Now()
	resp, err := s.r.Upload(ctx, s.gen, s.st.BaseVersion, dir, archive.Excluded)
	if err != nil {
		if isDefinitive(err) || ctx.Err() != nil {
			return err
		}
		// senza risposta non si sa se il commit c'è stato: lo dice lo stato
		s.reconcile(ctx)
		return err
	}
	s.st.BaseVersion = resp.Version.Number
	s.lastFP = fp
	if err := s.saveState(); err != nil {
		s.log.Warn("stato locale non salvato", "errore", err)
	}
	s.log.Info("mondo salvato",
		"versione", resp.Version.Number, "nuovi_byte", resp.AddedBytes,
		"file", resp.Files, "durata", time.Since(start).Round(time.Millisecond))
	return nil
}

// reconcile: dopo un upload senza risposta, se la versione corrente è stata
// fatta da noi con questa generazione, il commit c'è stato e la nostra base è
// quella. Solo noi (host con questa generazione) possiamo averla fatta.
func (s *session) reconcile(ctx context.Context) {
	st, err := s.status(ctx)
	if err != nil || st.Current == nil {
		return
	}
	c := st.Current
	if c.Device == s.device && c.Generation == s.gen && c.Number > s.st.BaseVersion {
		s.log.Info("l'upload senza risposta era andato a buon fine", "versione", c.Number)
		s.st.BaseVersion = c.Number
		if err := s.saveState(); err != nil {
			s.log.Warn("stato locale non salvato", "errore", err)
		}
	}
}

func (s *session) saveState() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(statePath(s.o.WorkDir), b, 0o600)
}

// ---------------------------------------------------------------------------
// File

// fingerprint è un'impronta economica del mondo (nomi, dimensioni, date di
// modifica dei file non esclusi): serve a saltare un upload quando non è
// cambiato niente. Un'impronta diversa non prova che il contenuto sia diverso;
// una uguale prova (a meno di file riscritti nello stesso istante con la
// stessa dimensione) che non lo è.
func fingerprint(dir string) string {
	h := sha256.New()
	var rows []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // un file sparito durante la lettura: l'impronta cambierà
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if archive.Excluded(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				rows = append(rows, fmt.Sprintf("%s|%d|%d", rel, info.Size(), info.ModTime().UnixNano()))
			}
		}
		return nil
	})
	sort.Strings(rows)
	for _, r := range rows {
		io.WriteString(h, r+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// refreshMirror rifà il mirror da zero come copia di src (file non esclusi,
// con la data di modifica). I file spariti mentre si copia si saltano.
func refreshMirror(src, mirror string) error {
	if err := os.RemoveAll(mirror); err != nil {
		return err
	}
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if archive.Excluded(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dst := filepath.Join(mirror, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, 0o755)
		case d.Type().IsRegular():
			err := copyFile(p, dst)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		return nil
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

func nonEmptyDir(dir string) bool {
	ents, err := os.ReadDir(dir)
	return err == nil && len(ents) > 0
}

// renameRetry rinomina una cartella riprovando qualche volta: su Windows un
// antivirus o l'indicizzazione possono tenerla aperta per un attimo.
func renameRetry(from, to string) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}
