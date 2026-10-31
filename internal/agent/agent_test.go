package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FedericoArdesi/mcsync/internal/api"
	"github.com/FedericoArdesi/mcsync/internal/archive"
	"github.com/FedericoArdesi/mcsync/internal/client"
	"github.com/FedericoArdesi/mcsync/internal/tarutil"
)

// fakeRemote è un servizio in memoria: tiene le versioni come flussi di
// tarutil.Pack (quindi prova anche Pack/Unpack veri) e fa fallire a comando.
type fakeRemote struct {
	mu sync.Mutex

	player, device string
	gen            uint64
	pointer        uint64
	versions       map[uint64][]byte
	lastAuthor     api.Version

	acquireErr   error
	heartbeatErr error
	// uploadErr decide per ogni upload (n da 1): se restituisce un errore
	// l'upload fallisce. Con commitAnyway il commit avviene lo stesso
	// (risposta persa).
	uploadErr    func(n int) error
	commitAnyway bool

	uploads     int
	parents     []uint64
	released    bool
	heartbeats  int
	downloadErr error
}

func newFake() *fakeRemote {
	return &fakeRemote{player: "Fede", device: "dev-1", gen: 7, versions: map[uint64][]byte{}}
}

func apiErr(code string) error {
	return &client.APIError{ErrorBody: api.ErrorBody{Code: code, Message: "prova"}, HTTPStatus: 409}
}

func (f *fakeRemote) Status(context.Context) (api.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := api.Status{Protocol: 1, You: api.Identity{Player: f.player, Device: f.device}, Pointer: f.pointer}
	if f.pointer != 0 {
		v := f.lastAuthor
		v.Number = f.pointer
		st.Current = &v
	}
	return st, nil
}

func (f *fakeRemote) Acquire(context.Context) (api.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acquireErr != nil {
		return api.Lease{}, f.acquireErr
	}
	return api.Lease{Player: f.player, Device: f.device, Generation: f.gen, ExpiresInSeconds: 300}, nil
}

func (f *fakeRemote) Heartbeat(_ context.Context, gen uint64) (api.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats++
	if f.heartbeatErr != nil {
		return api.Lease{}, f.heartbeatErr
	}
	return api.Lease{Generation: gen}, nil
}

func (f *fakeRemote) Release(context.Context, uint64, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = true
	return nil
}

func (f *fakeRemote) Upload(_ context.Context, gen, parent uint64, dir string, skip func(string) bool) (api.UploadResponse, error) {
	var buf bytes.Buffer
	if err := tarutil.Pack(&buf, dir, skip); err != nil {
		return api.UploadResponse{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads++
	f.parents = append(f.parents, parent)
	var uerr error
	if f.uploadErr != nil {
		uerr = f.uploadErr(f.uploads)
	}
	if uerr != nil && !f.commitAnyway {
		return api.UploadResponse{}, uerr
	}
	if parent != f.pointer {
		return api.UploadResponse{}, apiErr(api.CodeParentMismatch)
	}
	f.pointer++
	f.versions[f.pointer] = buf.Bytes()
	f.lastAuthor = api.Version{Number: f.pointer, Parent: parent, Device: f.device, Generation: gen}
	if uerr != nil {
		return api.UploadResponse{}, uerr
	}
	return api.UploadResponse{Version: f.lastAuthor, Files: 1}, nil
}

func (f *fakeRemote) Download(_ context.Context, version uint64, dst string, lim tarutil.Limits) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.downloadErr != nil {
		return 0, f.downloadErr
	}
	if version == 0 {
		version = f.pointer
	}
	b, ok := f.versions[version]
	if !ok {
		return 0, apiErr(api.CodeUnknownVersion)
	}
	return version, tarutil.Unpack(bytes.NewReader(b), dst, lim)
}

// seed mette in versions un mondo già salvato (versione 1).
func (f *fakeRemote) seed(t *testing.T, files map[string]string) {
	t.Helper()
	src := t.TempDir()
	for n, c := range files {
		p := filepath.Join(src, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := tarutil.Pack(&buf, src, archive.Excluded); err != nil {
		t.Fatal(err)
	}
	f.pointer = 1
	f.versions[1] = buf.Bytes()
	f.lastAuthor = api.Version{Number: 1, Device: "altro", Generation: 3}
}

func fastOpts(t *testing.T) Options {
	g := NewFakeGame()
	g.Every = 5 * time.Millisecond
	g.FileBytes = 256
	root := t.TempDir()
	return Options{
		World:          filepath.Join(root, "mondo"),
		Game:           g,
		UploadEvery:    40 * time.Millisecond,
		HeartbeatEvery: 10 * time.Millisecond,
		WarnAfter:      30 * time.Millisecond,
		StopAfter:      60 * time.Millisecond,
		CallTimeout:    time.Second,
		FinalAttempts:  2,
		FinalBackoff:   5 * time.Millisecond,
		StopTimeout:    time.Second,
	}
}

// runFor lancia Run e lo chiude con "Ctrl+C" dopo d, o prima se finisce da solo.
func runFor(t *testing.T, r Remote, o Options, d time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- Run(ctx, r, o) }()
	select {
	case err := <-res:
		return err
	case <-time.After(d):
		cancel()
	}
	select {
	case err := <-res:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run non è tornato dopo la chiusura")
		return nil
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFirstSessionSavesAndReleases(t *testing.T) {
	f := newFake()
	o := fastOpts(t)
	if err := runFor(t, f, o, 150*time.Millisecond); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.pointer == 0 {
		t.Fatal("nessuna versione salvata")
	}
	if !f.released {
		t.Error("lease non rilasciata dopo il salvataggio finale")
	}
	st, err := ReadState(o.workDir())
	if err != nil {
		t.Fatal(err)
	}
	if st.Dirty || st.BaseVersion != f.pointer {
		t.Errorf("stato locale = %+v, attesa base %d e non dirty", st, f.pointer)
	}
	// la versione finale contiene il mondo com'era alla chiusura
	chk := t.TempDir()
	if _, err := f.Download(context.Background(), 0, filepath.Join(chk, "w"), tarutil.Limits{MaxFiles: 100, MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(chk, "w", "ticks.txt")) != readFile(t, filepath.Join(o.World, "ticks.txt")) {
		t.Error("l'ultima versione non coincide con il mondo locale")
	}
}

func (o Options) workDir() string {
	if o.WorkDir != "" {
		return o.WorkDir
	}
	return o.World + ".mcsync"
}

func TestDownloadsLatestThenSavesOnTop(t *testing.T) {
	f := newFake()
	f.seed(t, map[string]string{"level.dat": "originale", "region/r.9.9.mca": "xyz"})
	o := fastOpts(t)
	if err := runFor(t, f, o, 150*time.Millisecond); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := readFile(t, filepath.Join(o.World, "level.dat")); got != "originale" {
		t.Errorf("level.dat = %q", got)
	}
	if got := readFile(t, filepath.Join(o.World, "region", "r.9.9.mca")); got != "xyz" {
		t.Errorf("file scaricato = %q", got)
	}
	if f.pointer < 2 {
		t.Fatalf("puntatore = %d, attesa almeno la versione 2", f.pointer)
	}
	if f.parents[0] != 1 {
		t.Errorf("il primo upload doveva partire dalla versione 1, parent = %d", f.parents[0])
	}
}

func TestDirtyLocalCopyGoesToRecoveryNotDeleted(t *testing.T) {
	f := newFake()
	f.seed(t, map[string]string{"level.dat": "ufficiale"})
	f.pointer = 2
	f.versions[2] = f.versions[1]
	o := fastOpts(t)
	// copia locale non confermata, basata sulla versione 1
	if err := os.MkdirAll(o.World, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.World, "mia-costruzione.txt"), []byte("importante"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(o.workDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(o.workDir()), []byte(`{"base_version":1,"dirty":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runFor(t, f, o, 100*time.Millisecond); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var found bool
	_ = filepath.WalkDir(filepath.Join(o.workDir(), "recovery"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "mia-costruzione.txt" {
			found = readFile(t, p) == "importante"
		}
		return nil
	})
	if !found {
		t.Error("la copia locale non confermata non è finita in recovery")
	}
	if _, err := os.Stat(filepath.Join(o.World, "mia-costruzione.txt")); err == nil {
		t.Error("il file locale è rimasto nel mondo ufficiale")
	}
}

func TestLeaseHeldStopsBeforeTouchingWorld(t *testing.T) {
	f := newFake()
	f.acquireErr = apiErr(api.CodeLeaseHeld)
	o := fastOpts(t)
	err := runFor(t, f, o, time.Second)
	var he *LeaseHeldError
	if !errors.As(err, &he) {
		t.Fatalf("errore = %v, atteso LeaseHeldError", err)
	}
	if _, serr := os.Stat(o.World); serr == nil {
		t.Error("il mondo è stato creato anche senza lease")
	}
	if f.uploads != 0 {
		t.Error("upload senza lease")
	}
}

func TestLeaseLostOnHeartbeatStopsAndKeepsLocalCopy(t *testing.T) {
	f := newFake()
	f.heartbeatErr = apiErr(api.CodeStaleGeneration)
	o := fastOpts(t)
	o.UploadEvery = time.Hour // niente upload periodici: si prova solo il fermo
	err := runFor(t, f, o, time.Second)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("errore = %v, atteso ErrLeaseLost", err)
	}
	if f.uploads != 0 {
		t.Errorf("uploads = %d: chi ha perso la lease non deve caricare come se niente fosse", f.uploads)
	}
	if f.released {
		t.Error("rilasciata una lease che non è più nostra")
	}
	st, _ := ReadState(o.workDir())
	if !st.Dirty {
		t.Error("la copia locale deve restare segnata come non confermata")
	}
}

func TestIsolationStopsGameAndDoesNotRelease(t *testing.T) {
	f := newFake()
	f.heartbeatErr = errors.New("rete caduta")
	o := fastOpts(t)
	o.UploadEvery = time.Hour
	err := runFor(t, f, o, 2*time.Second)
	if !errors.Is(err, ErrIsolated) {
		t.Fatalf("errore = %v, atteso ErrIsolated", err)
	}
	select {
	case <-o.Game.Done():
	default:
		t.Error("il gioco non è stato fermato")
	}
	if f.released || f.uploads != 0 {
		t.Errorf("released=%v uploads=%d: da isolato non si rilascia né si carica", f.released, f.uploads)
	}
}

func TestFinalUploadFailureKeepsLeaseAndDirty(t *testing.T) {
	f := newFake()
	f.uploadErr = func(int) error { return errors.New("rete caduta") }
	o := fastOpts(t)
	err := runFor(t, f, o, 150*time.Millisecond)
	if !errors.Is(err, ErrNotSaved) {
		t.Fatalf("errore = %v, atteso ErrNotSaved", err)
	}
	if f.released {
		t.Error("lease rilasciata senza conferma del commit finale")
	}
	st, _ := ReadState(o.workDir())
	if !st.Dirty {
		t.Error("il mondo locale deve restare dirty")
	}
	if f.pointer != 0 {
		t.Error("è nata una versione nonostante il fallimento")
	}
}

func TestUnansweredUploadIsReconciledFromStatus(t *testing.T) {
	f := newFake()
	f.commitAnyway = true
	// il primo upload fa il commit ma la risposta si perde
	f.uploadErr = func(n int) error {
		if n == 1 {
			return errors.New("connessione interrotta")
		}
		return nil
	}
	o := fastOpts(t)
	if err := runFor(t, f, o, 250*time.Millisecond); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.parents) < 2 {
		t.Fatalf("servono almeno due upload, ce ne sono %d", len(f.parents))
	}
	if f.parents[1] != 1 {
		t.Errorf("il secondo upload doveva ripartire dalla versione 1 (commit riconosciuto), parent = %d", f.parents[1])
	}
	if !f.released {
		t.Error("lease non rilasciata a fine sessione")
	}
}

func TestDownloadFailureLeavesLocalWorldAndReleases(t *testing.T) {
	f := newFake()
	f.seed(t, map[string]string{"level.dat": "x"})
	f.downloadErr = errors.New("rete caduta")
	o := fastOpts(t)
	if err := os.MkdirAll(o.World, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.World, "locale.txt"), []byte("mio"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runFor(t, f, o, time.Second)
	if err == nil {
		t.Fatal("atteso un errore")
	}
	if readFile(t, filepath.Join(o.World, "locale.txt")) != "mio" {
		t.Error("il mondo locale è stato toccato nonostante il download fallito")
	}
	if !f.released {
		t.Error("lease non rilasciata dopo un avvio fallito")
	}
}
