package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcsync/internal/protocol"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock { return &clock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)} }

func statePath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "state", "state.json")
}

func mustOpen(t *testing.T, path string, clk protocol.Clock) *protocol.Coordinator {
	t.Helper()
	c, err := Open(path, clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func text(t *testing.T, st protocol.State) string {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func req(l protocol.Lease, parent uint64, snap string) protocol.CommitRequest {
	return protocol.CommitRequest{
		Player: l.Player, Device: l.Device, Generation: l.Generation,
		Parent: parent, BlocksVerified: true, SizeBytes: 1, Snapshot: snap,
	}
}

func TestFirstStartHasNoFileAndStartsEmpty(t *testing.T) {
	path := statePath(t)
	if _, found, err := New(path).Load(); found || err != nil {
		t.Fatalf("primo avvio: found=%v err=%v", found, err)
	}
	c := mustOpen(t, path, newClock())
	if c.Pointer() != 0 || len(c.Versions()) != 0 {
		t.Fatal("il primo avvio doveva essere vuoto")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("senza modifiche non serve nessun file")
	}
	if _, err := c.Acquire("A", "pcA"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("dopo la prima modifica il file doveva esistere: %v", err)
	}
}

func TestRestartKeepsLeaseVersionsAndPointer(t *testing.T) {
	path, clk := statePath(t), newClock()
	c := mustOpen(t, path, clk)
	a, _ := c.Acquire("A", "pcA")
	if _, err := c.Commit(req(a, 0, "snap-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Commit(req(a, 1, "snap-2")); err != nil {
		t.Fatal(err)
	}
	before := text(t, c.State())

	c2 := mustOpen(t, path, clk) // riavvio
	if got := text(t, c2.State()); got != before {
		t.Fatalf("stato diverso dopo il riavvio:\n%s\n---\n%s", got, before)
	}
	if c2.Pointer() != 2 {
		t.Fatalf("puntatore = %d, atteso 2", c2.Pointer())
	}
	if v, ok := c2.Version(2); !ok || v.Snapshot != "snap-2" || v.Parent != 1 {
		t.Fatalf("versione 2 sbagliata: %+v %v", v, ok)
	}
}

func TestHostKeepsItsLeaseAcrossRestart(t *testing.T) {
	path, clk := statePath(t), newClock()
	c := mustOpen(t, path, clk)
	a, _ := c.Acquire("A", "pcA")
	clk.Advance(5 * time.Minute)

	c2 := mustOpen(t, path, clk) // il servizio si riavvia, l'host non se ne accorge
	if _, err := c2.Heartbeat("A", "pcA", a.Generation); err != nil {
		t.Fatalf("l'heartbeat dell'host doveva funzionare dopo il riavvio: %v", err)
	}
	if _, err := c2.Commit(req(a, 0, "snap-1")); err != nil {
		t.Fatalf("il commit dell'host doveva funzionare dopo il riavvio: %v", err)
	}
	if _, err := c2.Acquire("B", "pcB"); !errors.Is(err, protocol.ErrHeld) {
		t.Fatalf("B doveva trovare la lease occupata, ottenuto %v", err)
	}
}

func TestExpiredLeaseStaysExpiredAndGenerationNeverGoesBack(t *testing.T) {
	path, clk := statePath(t), newClock()
	c := mustOpen(t, path, clk)
	a, _ := c.Acquire("A", "pcA")
	// il servizio resta spento oltre la durata della lease
	clk.Advance(protocol.LeaseTTL + time.Minute)

	c2 := mustOpen(t, path, clk)
	if _, ok := c2.CurrentLease(); ok {
		t.Fatal("la lease scaduta non doveva tornare valida")
	}
	if _, err := c2.Heartbeat("A", "pcA", a.Generation); !errors.Is(err, protocol.ErrLeaseExpired) {
		t.Fatalf("atteso ErrLeaseExpired, ottenuto %v", err)
	}
	b, err := c2.Acquire("B", "pcB")
	if err != nil || b.Generation != 2 {
		t.Fatalf("B doveva prendere la generazione 2: %+v, %v", b, err)
	}
	// e il vecchio host viene respinto anche dopo un secondo riavvio
	c3 := mustOpen(t, path, clk)
	if _, err := c3.Commit(req(a, 0, "snap-vecchio")); !errors.Is(err, protocol.ErrStaleGeneration) {
		t.Fatalf("atteso ErrStaleGeneration, ottenuto %v", err)
	}
}

func TestHandoffReservationSurvivesRestart(t *testing.T) {
	path, clk := statePath(t), newClock()
	c := mustOpen(t, path, clk)
	a, _ := c.Acquire("A", "pcA")
	if _, err := c.RequestHandoff("B", "pcB"); err != nil {
		t.Fatal(err)
	}
	if err := c.Release("A", "pcA", a.Generation, "B"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)

	c2 := mustOpen(t, path, clk)
	if _, err := c2.Acquire("C", "pcC"); !errors.Is(err, protocol.ErrReserved) {
		t.Fatalf("C: atteso ErrReserved, ottenuto %v", err)
	}
	if b, err := c2.Acquire("B", "pcB"); err != nil || b.Generation != 2 {
		t.Fatalf("B doveva prendere la lease riservata: %+v, %v", b, err)
	}
}

func TestOrphansSurviveRestartWithTheirReason(t *testing.T) {
	path, clk := statePath(t), newClock()
	c := mustOpen(t, path, clk)
	a, _ := c.Acquire("A", "pcA")
	clk.Advance(protocol.LeaseTTL + time.Second)
	if _, err := c.Acquire("B", "pcB"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Commit(req(a, 0, "snap-di-A")); !errors.Is(err, protocol.ErrStaleGeneration) {
		t.Fatalf("atteso ErrStaleGeneration, ottenuto %v", err)
	}

	c2 := mustOpen(t, path, clk)
	o := c2.Orphans()
	if len(o) != 1 || o[0].Request.Snapshot != "snap-di-A" || !errors.Is(o[0].Reason, protocol.ErrStaleGeneration) {
		t.Fatalf("orfano perso o cambiato: %+v", o)
	}
}

func TestSaveFailureLeavesStateUntouched(t *testing.T) {
	// il servizio parte regolarmente, poi il percorso diventa inutilizzabile:
	// al posto della cartella dello stato compare un file normale
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "state.json")
	c, err := Open(path, newClock(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire("A", "pcA"); !errors.Is(err, protocol.ErrPersist) {
		t.Fatalf("atteso ErrPersist, ottenuto %v", err)
	}
	if _, ok := c.CurrentLease(); ok {
		t.Fatal("senza salvataggio non deve esistere nessuna lease")
	}
}

func TestSaveLeavesNoTempFile(t *testing.T) {
	path, clk := statePath(t), newClock()
	c := mustOpen(t, path, clk)
	a, _ := c.Acquire("A", "pcA")
	for i := 0; i < 5; i++ {
		if _, err := c.Heartbeat("A", "pcA", a.Generation); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("nella cartella doveva restare solo state.json, ci sono %d voci", len(entries))
	}
}

// Un guasto a metà salvataggio lascia al massimo un .tmp con dentro un pezzo di
// file: il file vero non va toccato e al riavvio il .tmp non conta.
func TestHalfWrittenTempFileIsIgnored(t *testing.T) {
	path, clk := statePath(t), newClock()
	c := mustOpen(t, path, clk)
	a, _ := c.Acquire("A", "pcA")
	before := text(t, c.State())

	if err := os.WriteFile(path+".tmp", []byte(`{"format":1,"generation":9`), 0o600); err != nil {
		t.Fatal(err)
	}
	c2 := mustOpen(t, path, clk)
	if got := text(t, c2.State()); got != before {
		t.Fatalf("il .tmp ha cambiato lo stato:\n%s\n---\n%s", got, before)
	}
	// e il salvataggio successivo lo sostituisce senza problemi
	if _, err := c2.Heartbeat("A", "pcA", a.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("il .tmp doveva sparire dopo il salvataggio")
	}
}

func TestCorruptFileIsNotReplacedWithEmptyState(t *testing.T) {
	cases := map[string]string{
		"file vuoto":          "",
		"JSON rotto":          `{"format":1,"generation":`,
		"formato sconosciuto": `{"format":42,"generation":1,"pointer":0,"versions":[],"orphans":[]}`,
		"stato incoerente":    `{"format":1,"generation":0,"pointer":3,"versions":[],"orphans":[]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := statePath(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Open(path, newClock(), nil)
			if !errors.Is(err, protocol.ErrBadState) || c != nil {
				t.Fatalf("atteso ErrBadState e nessun coordinatore, ottenuto %v, %v", c, err)
			}
			// il file non è stato toccato
			if got, _ := os.ReadFile(path); string(got) != content {
				t.Fatal("il file danneggiato non doveva essere sovrascritto")
			}
		})
	}
}

// Test differenziale: la stessa sequenza di operazioni va su un coordinatore
// che non si riavvia mai e su uno che si riavvia dal disco prima di OGNI
// operazione. Risposte e stato devono restare identici, passo per passo.
func TestRestartBeforeEveryOperationChangesNothing(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			clk := newClock()
			path := statePath(t)

			ref := protocol.NewCoordinator(clk, nil) // mai riavviato
			players := []string{"A", "B", "C"}
			gen := map[string]uint64{}
			devOf := func(p string) string { return "pc" + p }

			for step := 0; step < 60; step++ {
				dut := mustOpen(t, path, clk) // riavvio prima di ogni operazione

				p := players[rng.Intn(len(players))]
				var errRef, errDut error
				op := ""
				switch rng.Intn(8) {
				case 0:
					d := []time.Duration{10 * time.Second, time.Minute, 4 * time.Minute, 9 * time.Minute, 11 * time.Minute}[rng.Intn(5)]
					clk.Advance(d)
					op = fmt.Sprintf("avanza %v", d)
				case 1:
					op = "acquire " + p
					l1, e1 := ref.Acquire(p, devOf(p))
					l2, e2 := dut.Acquire(p, devOf(p))
					errRef, errDut = e1, e2
					if e1 == nil {
						gen[p] = l1.Generation
					}
					if (e1 == nil) != (e2 == nil) || l1.Generation != l2.Generation || !l1.ExpiresAt.Equal(l2.ExpiresAt) {
						t.Fatalf("passo %d (%s): lease diverse %+v / %+v", step, op, l1, l2)
					}
				case 2:
					op = "heartbeat " + p
					_, errRef = ref.Heartbeat(p, devOf(p), gen[p])
					_, errDut = dut.Heartbeat(p, devOf(p), gen[p])
				case 3, 4:
					op = "commit " + p
					parent := ref.Pointer()
					if rng.Intn(4) == 0 && parent > 0 {
						parent-- // base vecchia: diventa un orfano
					}
					r := protocol.CommitRequest{Player: p, Device: devOf(p), Generation: gen[p], Parent: parent, BlocksVerified: true, SizeBytes: 1, Snapshot: fmt.Sprintf("snap-%d", step)}
					_, errRef = ref.Commit(r)
					_, errDut = dut.Commit(r)
				case 5:
					op = "release " + p
					to := ""
					if rng.Intn(2) == 0 {
						to = players[rng.Intn(len(players))]
					}
					errRef = ref.Release(p, devOf(p), gen[p], to)
					errDut = dut.Release(p, devOf(p), gen[p], to)
				case 6:
					op = "request handoff " + p
					_, errRef = ref.RequestHandoff(p, devOf(p))
					_, errDut = dut.RequestHandoff(p, devOf(p))
				case 7:
					op = "cancel handoff " + p
					errRef = ref.CancelHandoff(p, devOf(p))
					errDut = dut.CancelHandoff(p, devOf(p))
				}
				if fmt.Sprint(errRef) != fmt.Sprint(errDut) {
					t.Fatalf("passo %d (%s): errori diversi: %v / %v", step, op, errRef, errDut)
				}
				if a, b := text(t, ref.State()), text(t, dut.State()); a != b {
					t.Fatalf("passo %d (%s): stati diversi:\n%s\n---\n%s", step, op, a, b)
				}
			}
		})
	}
}
