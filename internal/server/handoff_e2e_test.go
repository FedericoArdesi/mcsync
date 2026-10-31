package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FedericoArdesi/mcsync/internal/agent"
	"github.com/FedericoArdesi/mcsync/internal/api"
	apiclient "github.com/FedericoArdesi/mcsync/internal/client"
)

func e2eOptions(t *testing.T) agent.Options {
	t.Helper()
	g := agent.NewFakeGame()
	g.Every = 5 * time.Millisecond
	g.FileBytes = 256
	return agent.Options{
		World:          filepath.Join(t.TempDir(), "mondo"),
		Game:           g,
		UploadEvery:    time.Hour, // niente upload periodici: contano quelli di chiusura
		HeartbeatEvery: 20 * time.Millisecond,
		CallTimeout:    5 * time.Second,
		FinalAttempts:  2,
		FinalBackoff:   5 * time.Millisecond,
		StopTimeout:    5 * time.Second,
	}
}

func waitUntil(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("troppo tempo ad aspettare: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Il passaggio di host con servizio vero, HTTP vero e due agenti veri: Fede
// gioca, Ale chiede, Fede salva e rilascia da sola, Ale riparte dalla versione
// di Fede e salva la sua sopra.
func TestHandoffBetweenTwoRealAgents(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	gio := e.enroll("Gio")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// --- Fede gioca ---
	fo := e2eOptions(t)
	fedeRes := make(chan error, 1)
	go func() { fedeRes <- agent.Run(ctx, apiclient.New(e.ts.URL, fede.token), fo) }()
	waitUntil(t, "il gioco di Fede scrive il mondo", 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(fo.World, "ticks.txt"))
		return err == nil
	})

	// --- Ale chiede: Fede deve chiudersi da sola ---
	ale.mustRequestHandoff()
	select {
	case err := <-fedeRes:
		if err != nil {
			t.Fatalf("Fede: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Fede non ha passato la mano dopo la richiesta")
	}

	st, _, _ := ale.status()
	if st.Pointer != 1 || st.Lease != nil || st.Handoff != nil {
		t.Fatalf("dopo il passaggio: versione %d, lease %+v, richiesta %+v (attesa versione 1, nessuna lease, nessuna richiesta)", st.Pointer, st.Lease, st.Handoff)
	}
	if st.Reservation == nil || st.Reservation.For != "Ale" {
		t.Fatalf("la lease doveva essere riservata ad Ale: %+v", st.Reservation)
	}
	if v, _ := e.coord.Version(1); v.Author != "Fede" {
		t.Fatalf("la versione 1 doveva essere di Fede: %+v", v)
	}
	// nessun altro può infilarsi
	if _, code, eb := gio.acquire(); code != 409 || eb.Code != api.CodeLeaseReserved {
		t.Fatalf("Gio: atteso 409 lease_reserved, ottenuto %d %v", code, eb)
	}
	fedeTicks, err := os.ReadFile(filepath.Join(fo.World, "ticks.txt"))
	if err != nil {
		t.Fatal(err)
	}

	// --- Ale riparte dalla versione di Fede ---
	ao := e2eOptions(t)
	aleCtx, aleCancel := context.WithCancel(ctx)
	defer aleCancel()
	aleRes := make(chan error, 1)
	go func() { aleRes <- agent.Run(aleCtx, apiclient.New(e.ts.URL, ale.token), ao) }()
	waitUntil(t, "Ale scarica la versione 1", 10*time.Second, func() bool {
		s, err := agent.ReadState(ao.World + ".mcsync")
		return err == nil && s.BaseVersion == 1
	})
	time.Sleep(100 * time.Millisecond) // il gioco di Ale scrive qualcosa
	aleCancel()                        // "Ctrl+C"
	select {
	case err := <-aleRes:
		if err != nil {
			t.Fatalf("Ale: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Ale non si è chiusa")
	}

	if e.coord.Pointer() != 2 {
		t.Fatalf("versione ufficiale = %d, attesa 2", e.coord.Pointer())
	}
	v2, _ := e.coord.Version(2)
	if v2.Parent != 1 || v2.Author != "Ale" || v2.Generation != 2 {
		t.Fatalf("versione 2 = %+v, attesa figlia della 1, di Ale, generazione 2", v2)
	}
	if st, _, _ := gio.status(); st.Lease != nil || st.Reservation != nil {
		t.Fatalf("alla fine nessuno deve tenere lease o riserva: %+v %+v", st.Lease, st.Reservation)
	}
	// la versione 1 è esattamente il mondo con cui Fede ha chiuso
	code, files, _, eb := ale.download("?version=1")
	if code != 200 {
		t.Fatalf("download versione 1: %d %v", code, eb)
	}
	if files["ticks.txt"] != string(fedeTicks) {
		t.Fatalf("la versione 1 non coincide con il mondo di Fede: %q contro %q", files["ticks.txt"], fedeTicks)
	}
}
