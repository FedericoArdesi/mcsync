package protocol

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type fakeSaver struct {
	saves int
	last  State
	fail  error
}

func (f *fakeSaver) Save(st State) error {
	if f.fail != nil {
		return f.fail
	}
	f.saves++
	f.last = st
	return nil
}

func newPersistentTest(t *testing.T) (*Coordinator, *fakeClock, *fakeSaver) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	s := &fakeSaver{}
	c, err := NewPersistent(clk, nil, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, clk, s
}

func TestSaverCalledOnEveryChange(t *testing.T) {
	c, _, s := newPersistentTest(t)
	step := func(name string, want int) {
		t.Helper()
		if s.saves != want {
			t.Fatalf("dopo %s: salvataggi = %d, attesi %d", name, s.saves, want)
		}
	}
	a, _ := c.Acquire("A", "pcA")
	step("Acquire", 1)
	_, _ = c.Acquire("A", "pcA") // rinnovo
	step("rinnovo con Acquire", 2)
	_, _ = c.Heartbeat("A", "pcA", a.Generation)
	step("Heartbeat", 3)
	_, _ = c.Commit(commitReq(a, 0))
	step("Commit", 4)
	_, _ = c.Commit(commitReq(a, 0)) // rifiutato: diventa orfano
	step("commit rifiutato (orfano)", 5)
	_ = c.Release("A", "pcA", a.Generation, "")
	step("Release", 6)

	// quello che non cambia niente non salva
	_, _ = c.Acquire("B", "pcB") // libera: questa cambia
	step("Acquire di B", 7)
	_, _ = c.Acquire("C", "pcC") // ErrHeld
	_, _ = c.Heartbeat("A", "pcA", a.Generation)
	req := commitReq(a, 1)
	req.BlocksVerified = false
	_, _ = c.Commit(req) // blocchi incompleti: niente orfano
	_ = c.Release("C", "pcC", 99, "")
	step("richieste respinte senza modifiche", 7)
}

func TestSavedStateMatchesMemory(t *testing.T) {
	c, _, s := newPersistentTest(t)
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.Commit(commitReq(a, 0))
	_, _ = c.Commit(commitReq(a, 0))
	if got, want := stateText(t, s.last), stateText(t, c.State()); got != want {
		t.Fatalf("lo stato salvato è diverso da quello in memoria:\n%s\n---\n%s", got, want)
	}
}

func TestSaveFailureRollsBackAcquire(t *testing.T) {
	c, _, s := newPersistentTest(t)
	s.fail = errors.New("disco pieno")
	if _, err := c.Acquire("A", "pcA"); !errors.Is(err, ErrPersist) {
		t.Fatalf("atteso ErrPersist, ottenuto %v", err)
	}
	if _, ok := c.CurrentLease(); ok {
		t.Fatal("la lease non doveva esistere")
	}
	s.fail = nil
	l, err := c.Acquire("A", "pcA")
	if err != nil || l.Generation != 1 {
		t.Fatalf("la generazione non doveva essere consumata: %+v, %v", l, err)
	}
}

func TestSaveFailureRollsBackHeartbeat(t *testing.T) {
	c, clk, s := newPersistentTest(t)
	a, _ := c.Acquire("A", "pcA")
	clk.Advance(5 * time.Minute)
	s.fail = errors.New("disco pieno")
	if _, err := c.Heartbeat("A", "pcA", a.Generation); !errors.Is(err, ErrPersist) {
		t.Fatalf("atteso ErrPersist, ottenuto %v", err)
	}
	if l, _ := c.CurrentLease(); !l.ExpiresAt.Equal(a.ExpiresAt) {
		t.Fatalf("la scadenza doveva restare %v, è %v", a.ExpiresAt, l.ExpiresAt)
	}
}

func TestSaveFailureRollsBackCommit(t *testing.T) {
	c, _, s := newPersistentTest(t)
	a, _ := c.Acquire("A", "pcA")
	s.fail = errors.New("disco pieno")
	if _, err := c.Commit(commitReq(a, 0)); !errors.Is(err, ErrPersist) {
		t.Fatalf("atteso ErrPersist, ottenuto %v", err)
	}
	if c.Pointer() != 0 || len(c.Versions()) != 0 {
		t.Fatal("senza salvataggio la versione non deve esistere")
	}
	s.fail = nil
	v, err := c.Commit(commitReq(a, 0))
	if err != nil || v.Number != 1 {
		t.Fatalf("riprovando doveva passare con numero 1: %+v, %v", v, err)
	}
}

func TestSaveFailureRollsBackRelease(t *testing.T) {
	c, _, s := newPersistentTest(t)
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.RequestHandoff("B", "pcB")
	s.fail = errors.New("disco pieno")
	if err := c.Release("A", "pcA", a.Generation, "B"); !errors.Is(err, ErrPersist) {
		t.Fatalf("atteso ErrPersist, ottenuto %v", err)
	}
	if _, ok := c.CurrentLease(); !ok {
		t.Fatal("la lease doveva restare di A")
	}
	if _, err := c.Acquire("C", "pcC"); !errors.Is(err, ErrHeld) {
		t.Fatalf("senza riserva e con la lease di A, C trova ErrHeld; ottenuto %v", err)
	}
}

func TestRejectedCommitWithSaveFailureReportsBoth(t *testing.T) {
	c, _, s := newPersistentTest(t)
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.Commit(commitReq(a, 0))
	s.fail = errors.New("disco pieno")
	_, err := c.Commit(commitReq(a, 0)) // base vecchia
	if !errors.Is(err, ErrParentMismatch) || !errors.Is(err, ErrPersist) {
		t.Fatalf("atteso ErrParentMismatch e ErrPersist insieme, ottenuto %v", err)
	}
	if len(c.Orphans()) != 0 {
		t.Fatal("l'orfano non salvato non deve restare in memoria")
	}
}

func TestRestoreRejectsBadState(t *testing.T) {
	good := func() State {
		c, _, _ := newPersistentTest(t)
		a, _ := c.Acquire("A", "pcA")
		_, _ = c.Commit(commitReq(a, 0))
		_, _ = c.Commit(commitReq(a, 1))
		return c.State()
	}
	cases := map[string]func(*State){
		"formato sconosciuto":         func(s *State) { s.Format = 99 },
		"numeri di versione saltati":  func(s *State) { s.Versions[1].Number = 5 },
		"base nel futuro":             func(s *State) { s.Versions[1].Parent = 2 },
		"puntatore oltre le versioni": func(s *State) { s.Pointer = 7 },
		"generazione che torna indietro": func(s *State) {
			s.Generation = 0
		},
		"lease dal futuro": func(s *State) { s.Lease.Generation = s.Generation + 1 },
		"riserva senza nome": func(s *State) {
			s.Reservation = &Reservation{}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			st := good()
			mutate(&st)
			clk := &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
			if c, err := NewPersistent(clk, nil, nil, &st); !errors.Is(err, ErrBadState) || c != nil {
				t.Fatalf("atteso ErrBadState e nessun coordinatore, ottenuto %v, %v", c, err)
			}
		})
	}
}

func TestRestoreRebuildsKnownOrphanReasons(t *testing.T) {
	c, _, _ := newPersistentTest(t)
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.Commit(commitReq(a, 0))
	req := commitReq(a, 0)
	req.Snapshot = "snap-orfano"
	_, _ = c.Commit(req)

	st := c.State()
	clk := &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	c2, err := NewPersistent(clk, nil, nil, &st)
	if err != nil {
		t.Fatal(err)
	}
	o := c2.Orphans()
	if len(o) != 1 || !errors.Is(o[0].Reason, ErrParentMismatch) || o[0].Request.Snapshot != "snap-orfano" {
		t.Fatalf("orfano ricostruito male: %+v", o)
	}
}

func TestStateIsACopy(t *testing.T) {
	c, _, _ := newPersistentTest(t)
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.Commit(commitReq(a, 0))
	st := c.State()
	st.Versions[0].Author = "intruso"
	st.Lease.Player = "intruso"
	if v, _ := c.Version(1); v.Author != "A" {
		t.Fatal("modificare la copia ha cambiato il coordinatore")
	}
	if l, _ := c.CurrentLease(); l.Player != "A" {
		t.Fatal("modificare la copia ha cambiato la lease")
	}
}

// stateText rende lo stato in JSON, così il confronto non dipende da come
// sono rappresentati internamente gli orari.
func stateText(t *testing.T, st State) string {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
