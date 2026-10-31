package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcsync/internal/agent"
	"mcsync/internal/api"
	"mcsync/internal/archive"
	"mcsync/internal/auth"
	"mcsync/internal/client"
	"mcsync/internal/protocol"
	"mcsync/internal/server"
	"mcsync/internal/service"
)

// Il comando "mcsync handoff" contro il servizio vero: coordinatore vero,
// server HTTP vero, iscrizione con il comando enroll, agenti veri con il gioco
// finto. Orologio di sistema: i tempi dei test sono brevi rispetto alla lease.

type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type realEnv struct {
	t     *testing.T
	coord *protocol.Coordinator
	auth  *auth.Store
	ts    *httptest.Server
}

func newRealEnv(t *testing.T) *realEnv {
	t.Helper()
	root := t.TempDir()
	clk := protocol.SystemClock{}
	coord := protocol.NewCoordinator(clk, nil)
	devs, err := auth.Open(filepath.Join(root, "devices.json"), clk)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{
		Coordinator: coord,
		Service:     service.New(coord, archive.NewMemStore()),
		Auth:        devs,
		Clock:       clk,
		WorkDir:     filepath.Join(root, "work"),
		Logger:      slog.New(slog.NewTextHandler(&safeBuf{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &realEnv{t: t, coord: coord, auth: devs, ts: ts}
}

// enroll iscrive un PC con il comando vero e restituisce il file di config.
func (e *realEnv) enroll(player string) string {
	e.t.Helper()
	secret, err := e.auth.CreateInvite(player)
	if err != nil {
		e.t.Fatal(err)
	}
	cfg := filepath.Join(e.t.TempDir(), player+".json")
	code := api.EncodeInviteCode(e.ts.Listener.Addr().String(), secret)
	c, o, er := cli(e.t, context.Background(), "enroll", code, "--label", "PC di "+player, "--config", cfg)
	if c != 0 {
		e.t.Fatalf("enroll %s: code=%d out=%q err=%q", player, c, o, er)
	}
	return cfg
}

// raw dà un client HTTP diretto, con il token letto dal file di config.
func (e *realEnv) raw(cfg string) *client.Client {
	e.t.Helper()
	c, _, err := connect(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

type session struct {
	out, errw *safeBuf
	done      chan int      // riceve il codice di uscita, una volta sola
	finished  chan struct{} // chiuso quando il comando è finito
	cancel    context.CancelFunc
}

// start lancia un comando in un goroutine; cancel è il "Ctrl+C".
func start(t *testing.T, args ...string) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{out: &safeBuf{}, errw: &safeBuf{}, done: make(chan int, 1), finished: make(chan struct{}), cancel: cancel}
	go func() {
		s.done <- run(ctx, args, s.out, s.errw)
		close(s.finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.finished:
		case <-time.After(10 * time.Second):
		}
	})
	return s
}

func (s *session) wait(t *testing.T, what string, d time.Duration) int {
	t.Helper()
	select {
	case c := <-s.done:
		return c
	case <-time.After(d):
		t.Fatalf("troppo tempo ad aspettare: %s\nout: %s\nerr: %s", what, s.out.String(), s.errw.String())
		return -1
	}
}

func until(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("troppo tempo ad aspettare: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// playingFrom aspetta che l'agente abbia scaricato la versione base e che il
// gioco abbia avuto il tempo di scrivere qualcosa: solo allora un Ctrl+C
// produce una versione nuova (come nel test del server).
func playingFrom(t *testing.T, world string, base uint64) {
	t.Helper()
	until(t, "l'agente scarica la versione di partenza", 10*time.Second, func() bool {
		s, err := agent.ReadState(world + ".mcsync")
		return err == nil && s.BaseVersion == base
	})
	time.Sleep(100 * time.Millisecond)
}

func (e *realEnv) holder() string {
	l, ok := e.coord.CurrentLease()
	if !ok {
		return ""
	}
	return l.Player
}

var fastFlags = []string{"--fake-every", "10ms", "--upload-every", "1h", "--heartbeat-every", "20ms"}

func runArgs(cfg, world string, extra ...string) []string {
	a := append([]string{"--config", cfg, "--world", world}, fastFlags...)
	return append(a, extra...)
}

// Passaggio completo: Fede gioca con "mcsync run", Ale lancia "mcsync handoff".
// Fede salva e rilascia da sola, Ale riparte dal mondo di Fede e salva sopra.
func TestHandoffCommandAgainstRealService(t *testing.T) {
	e := newRealEnv(t)
	fedeCfg, aleCfg := e.enroll("Fede"), e.enroll("Ale")

	fedeWorld := filepath.Join(t.TempDir(), "mondo")
	fede := start(t, append([]string{"run"}, runArgs(fedeCfg, fedeWorld)...)...)
	until(t, "Fede host", 10*time.Second, func() bool { return e.holder() == "Fede" })
	until(t, "il gioco di Fede scrive il mondo", 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(fedeWorld, "ticks.txt"))
		return err == nil
	})

	aleWorld := filepath.Join(t.TempDir(), "mondo")
	ale := start(t, append([]string{"handoff"}, runArgs(aleCfg, aleWorld, "--handoff-poll", "20ms", "--handoff-timeout", "20s")...)...)

	// Fede si chiude da sola, senza Ctrl+C
	if c := fede.wait(t, "Fede passa la mano", 15*time.Second); c != 0 {
		t.Fatalf("Fede: code=%d out=%q err=%q", c, fede.out.String(), fede.errw.String())
	}
	if !strings.Contains(fede.out.String(), "Fatto") {
		t.Errorf("Fede: out=%q", fede.out.String())
	}
	if v, ok := e.coord.Version(1); !ok || v.Author != "Fede" {
		t.Fatalf("la versione 1 doveva essere di Fede: %+v %v", v, ok)
	}

	// Ale prende la lease (generazione 2), ripartendo dalla versione 1
	until(t, "Ale host", 10*time.Second, func() bool { return e.holder() == "Ale" })
	if l, _ := e.coord.CurrentLease(); l.Generation != 2 {
		t.Fatalf("Ale doveva avere la generazione 2: %+v", l)
	}
	playingFrom(t, aleWorld, 1)
	if b, err := os.ReadFile(filepath.Join(aleWorld, "level.dat")); err != nil || string(b) != "mondo finto\n" {
		t.Fatalf("Ale non ha il mondo di Fede: %q %v", b, err)
	}
	if !strings.Contains(ale.out.String(), "ti ha passato la mano") {
		t.Errorf("Ale: manca l'avviso del passaggio: out=%q", ale.out.String())
	}

	ale.cancel() // "Ctrl+C"
	if c := ale.wait(t, "Ale si chiude", 15*time.Second); c != 0 {
		t.Fatalf("Ale: code=%d out=%q err=%q", c, ale.out.String(), ale.errw.String())
	}
	if p := e.coord.Pointer(); p != 2 {
		t.Fatalf("versione ufficiale = %d, attesa 2", p)
	}
	v2, _ := e.coord.Version(2)
	if v2.Parent != 1 || v2.Author != "Ale" || v2.Generation != 2 {
		t.Fatalf("versione 2 = %+v, attesa figlia della 1, di Ale, generazione 2", v2)
	}
	if e.holder() != "" {
		t.Fatalf("alla fine nessuno deve tenere la lease: %q", e.holder())
	}
}

// L'host non risponde (nessun agente): dopo il timeout la richiesta sparisce.
func TestHandoffCommandTimeoutWithdrawsRequest(t *testing.T) {
	e := newRealEnv(t)
	fedeCfg, aleCfg := e.enroll("Fede"), e.enroll("Ale")
	if _, err := e.raw(fedeCfg).Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}

	c, o, er := cli(t, context.Background(), "handoff", "--config", aleCfg, "--world", filepath.Join(t.TempDir(), "mondo"),
		"--handoff-timeout", "150ms", "--handoff-poll", "20ms")
	if c != 1 || !strings.Contains(er, "richiesta ritirata") || !strings.Contains(o, "Ho chiesto a Fede") {
		t.Fatalf("code=%d out=%q err=%q", c, o, er)
	}
	if _, ok := e.coord.PendingHandoff(); ok {
		t.Fatal("la richiesta doveva essere ritirata")
	}
	if e.holder() != "Fede" {
		t.Fatalf("la lease di Fede non doveva cambiare: %q", e.holder())
	}
}

// Ctrl+C mentre si aspetta: la richiesta viene ritirata anche se il contesto
// del comando è già annullato.
func TestHandoffCommandCtrlCWithdrawsRequest(t *testing.T) {
	e := newRealEnv(t)
	fedeCfg, aleCfg := e.enroll("Fede"), e.enroll("Ale")
	if _, err := e.raw(fedeCfg).Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}

	ale := start(t, "handoff", "--config", aleCfg, "--world", filepath.Join(t.TempDir(), "mondo"),
		"--handoff-timeout", "30s", "--handoff-poll", "20ms")
	until(t, "richiesta di Ale", 10*time.Second, func() bool {
		h, ok := e.coord.PendingHandoff()
		return ok && h.To == "Ale"
	})
	ale.cancel()
	if c := ale.wait(t, "Ale si ferma", 10*time.Second); c != 1 {
		t.Fatalf("code=%d out=%q err=%q", c, ale.out.String(), ale.errw.String())
	}
	if !strings.Contains(ale.out.String(), "Richiesta ritirata") {
		t.Errorf("out=%q", ale.out.String())
	}
	if _, ok := e.coord.PendingHandoff(); ok {
		t.Fatal("la richiesta doveva essere ritirata")
	}
}

// Una sola richiesta alla volta: chi arriva dopo riceve un messaggio chiaro e
// non tocca quella di chi era arrivato prima.
func TestHandoffCommandSecondRequesterIsTold(t *testing.T) {
	e := newRealEnv(t)
	fedeCfg, aleCfg, gioCfg := e.enroll("Fede"), e.enroll("Ale"), e.enroll("Gio")
	if _, err := e.raw(fedeCfg).Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}

	ale := start(t, "handoff", "--config", aleCfg, "--world", filepath.Join(t.TempDir(), "mondo"),
		"--handoff-timeout", "30s", "--handoff-poll", "20ms")
	until(t, "richiesta di Ale", 10*time.Second, func() bool {
		h, ok := e.coord.PendingHandoff()
		return ok && h.To == "Ale"
	})

	c, _, er := cli(t, context.Background(), "handoff", "--config", gioCfg, "--world", filepath.Join(t.TempDir(), "mondo"),
		"--handoff-timeout", "1s", "--handoff-poll", "20ms")
	if c != 1 || !strings.Contains(er, "già una richiesta di passaggio") {
		t.Fatalf("Gio: code=%d err=%q", c, er)
	}
	if h, ok := e.coord.PendingHandoff(); !ok || h.To != "Ale" {
		t.Fatalf("la richiesta di Ale doveva restare: %+v %v", h, ok)
	}
	ale.cancel()
	ale.wait(t, "Ale si ferma", 10*time.Second)
}

// Chi è già host non può chiedere il passaggio a se stesso.
func TestHandoffCommandAlreadyHostIsTold(t *testing.T) {
	e := newRealEnv(t)
	fedeCfg := e.enroll("Fede")
	if _, err := e.raw(fedeCfg).Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, _, er := cli(t, context.Background(), "handoff", "--config", fedeCfg, "--world", filepath.Join(t.TempDir(), "mondo"))
	if c != 1 || !strings.Contains(er, "sei già host") {
		t.Fatalf("code=%d err=%q", c, er)
	}
	if _, ok := e.coord.PendingHandoff(); ok {
		t.Fatal("nessuna richiesta doveva partire")
	}
}

// Passaggio a catena: Fede -> Ale -> Gio, ognuno con il comando vero. Ogni
// versione deve essere figlia della precedente e di chi ha chiuso, con la
// generazione che sale a ogni passaggio.
func TestHandoffCommandChain(t *testing.T) {
	e := newRealEnv(t)
	fedeCfg, aleCfg, gioCfg := e.enroll("Fede"), e.enroll("Ale"), e.enroll("Gio")

	fedeWorld := filepath.Join(t.TempDir(), "mondo")
	fede := start(t, append([]string{"run"}, runArgs(fedeCfg, fedeWorld)...)...)
	until(t, "Fede host", 10*time.Second, func() bool { return e.holder() == "Fede" })
	until(t, "il gioco di Fede scrive", 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(fedeWorld, "ticks.txt"))
		return err == nil
	})

	hand := func(cfg string) (*session, string) {
		w := filepath.Join(t.TempDir(), "mondo")
		return start(t, append([]string{"handoff"}, runArgs(cfg, w, "--handoff-poll", "20ms", "--handoff-timeout", "20s")...)...), w
	}

	ale, aleWorld := hand(aleCfg)
	if c := fede.wait(t, "Fede passa la mano ad Ale", 15*time.Second); c != 0 {
		t.Fatalf("Fede: code=%d err=%q", c, fede.errw.String())
	}
	until(t, "Ale host", 10*time.Second, func() bool { return e.holder() == "Ale" })
	playingFrom(t, aleWorld, 1)

	gio, gioWorld := hand(gioCfg)
	if c := ale.wait(t, "Ale passa la mano a Gio", 15*time.Second); c != 0 {
		t.Fatalf("Ale: code=%d out=%q err=%q", c, ale.out.String(), ale.errw.String())
	}
	until(t, "Gio host", 10*time.Second, func() bool { return e.holder() == "Gio" })
	if !strings.Contains(gio.out.String(), "Ale ha salvato e ti ha passato la mano") {
		t.Errorf("Gio: out=%q", gio.out.String())
	}

	playingFrom(t, gioWorld, 2)
	gio.cancel()
	if c := gio.wait(t, "Gio si chiude", 15*time.Second); c != 0 {
		t.Fatalf("Gio: code=%d out=%q err=%q", c, gio.out.String(), gio.errw.String())
	}

	want := []struct {
		author string
		gen    uint64
	}{{"Fede", 1}, {"Ale", 2}, {"Gio", 3}}
	if p := e.coord.Pointer(); p != uint64(len(want)) {
		t.Fatalf("versione ufficiale = %d, attesa %d", p, len(want))
	}
	for i, w := range want {
		v, ok := e.coord.Version(uint64(i + 1))
		if !ok || v.Author != w.author || v.Generation != w.gen || v.Parent != uint64(i) {
			t.Fatalf("versione %d = %+v, attesa di %s, generazione %d, figlia della %d", i+1, v, w.author, w.gen, i)
		}
	}
	if e.holder() != "" {
		t.Fatalf("alla fine nessuno deve tenere la lease: %q", e.holder())
	}
	if _, ok := e.coord.PendingHandoff(); ok {
		t.Fatal("nessuna richiesta doveva restare")
	}
}
