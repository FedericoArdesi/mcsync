package protocol

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRequestHandoffVisibleToHost(t *testing.T) {
	c, _ := newTest()
	a, _ := c.Acquire("A", "pcA")
	if _, ok := c.PendingHandoff(); ok {
		t.Fatal("non doveva esserci nessuna richiesta")
	}
	h, err := c.RequestHandoff("B", "pcB")
	if err != nil {
		t.Fatal(err)
	}
	if h.From != "A" || h.To != "B" || h.ToDevice != "pcB" {
		t.Fatalf("richiesta sbagliata: %+v", h)
	}
	// l'host la legge al heartbeat successivo e la lease resta sua
	if _, err := c.Heartbeat("A", "pcA", a.Generation); err != nil {
		t.Fatal(err)
	}
	got, ok := c.PendingHandoff()
	if !ok || got != h {
		t.Fatalf("PendingHandoff = %+v, %v", got, ok)
	}
}

func TestRequestHandoffNeedsLiveLease(t *testing.T) {
	c, clk := newTest()
	if _, err := c.RequestHandoff("B", "pcB"); !errors.Is(err, ErrNoLease) {
		t.Fatalf("senza lease: atteso ErrNoLease, ottenuto %v", err)
	}
	_, _ = c.Acquire("A", "pcA")
	clk.Advance(LeaseTTL + time.Second)
	if _, err := c.RequestHandoff("B", "pcB"); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("lease scaduta: atteso ErrLeaseExpired, ottenuto %v", err)
	}
}

func TestHostCannotRequestHandoff(t *testing.T) {
	c, _ := newTest()
	_, _ = c.Acquire("A", "pcA")
	for _, dev := range []string{"pcA", "portatileA"} {
		if _, err := c.RequestHandoff("A", dev); !errors.Is(err, ErrAlreadyHost) {
			t.Fatalf("dispositivo %s: atteso ErrAlreadyHost, ottenuto %v", dev, err)
		}
	}
}

func TestOnlyOneHandoffRequestAtATime(t *testing.T) {
	c, _ := newTest()
	_, _ = c.Acquire("A", "pcA")
	first, err := c.RequestHandoff("B", "pcB")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RequestHandoff("C", "pcC"); !errors.Is(err, ErrHandoffPending) {
		t.Fatalf("C: atteso ErrHandoffPending, ottenuto %v", err)
	}
	if _, err := c.RequestHandoff("B", "altroPC"); !errors.Is(err, ErrHandoffPending) {
		t.Fatalf("B da un altro dispositivo: atteso ErrHandoffPending, ottenuto %v", err)
	}
	// stessa richiesta ripetuta (nuovo tentativo dopo un errore di rete)
	again, err := c.RequestHandoff("B", "pcB")
	if err != nil || again != first {
		t.Fatalf("ripetizione: %+v, %v", again, err)
	}
}

func TestSimultaneousHandoffRequestsOneWinner(t *testing.T) {
	c, _ := newTest()
	_, _ = c.Acquire("A", "pcA")
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := c.RequestHandoff(fmt.Sprintf("p%d", i), fmt.Sprintf("dev%d", i))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, ErrHandoffPending):
				t.Errorf("errore inatteso: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("hanno vinto %d richieste, doveva vincerne una", wins)
	}
}

func TestCancelHandoffBeforeRelease(t *testing.T) {
	c, _ := newTest()
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.RequestHandoff("B", "pcB")

	if err := c.CancelHandoff("C", "pcC"); !errors.Is(err, ErrNotRequester) {
		t.Fatalf("C: atteso ErrNotRequester, ottenuto %v", err)
	}
	if err := c.CancelHandoff("B", "altroPC"); !errors.Is(err, ErrNotRequester) {
		t.Fatalf("B da un altro dispositivo: atteso ErrNotRequester, ottenuto %v", err)
	}
	if _, ok := c.PendingHandoff(); !ok {
		t.Fatal("la richiesta di B doveva restare")
	}
	if err := c.CancelHandoff("B", "pcB"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.PendingHandoff(); ok {
		t.Fatal("la richiesta doveva sparire")
	}
	// l'host continua come se niente fosse
	if _, err := c.Heartbeat("A", "pcA", a.Generation); err != nil {
		t.Fatalf("l'host doveva poter continuare: %v", err)
	}
	if err := c.CancelHandoff("B", "pcB"); !errors.Is(err, ErrNoHandoff) {
		t.Fatalf("seconda cancellazione: atteso ErrNoHandoff, ottenuto %v", err)
	}
	// e un altro può chiedere
	if _, err := c.RequestHandoff("C", "pcC"); err != nil {
		t.Fatalf("C doveva poter chiedere: %v", err)
	}
}

func TestHandoffFullFlow(t *testing.T) {
	c, clk := newTest()
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.RequestHandoff("B", "pcB")

	if err := c.Release("A", "pcA", a.Generation, "B"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.PendingHandoff(); ok {
		t.Fatal("dopo il rilascio la richiesta non deve restare")
	}
	if _, err := c.Acquire("C", "pcC"); !errors.Is(err, ErrReserved) {
		t.Fatalf("C: atteso ErrReserved, ottenuto %v", err)
	}
	clk.Advance(time.Minute)
	b, err := c.Acquire("B", "pcB")
	if err != nil || b.Generation != 2 {
		t.Fatalf("B doveva prendere la lease: %+v, %v", b, err)
	}
	if _, ok := c.PendingHandoff(); ok {
		t.Fatal("nessuna richiesta doveva essere rimasta")
	}
}

// 6.7: "Se il nuovo host si ritira prima di prendere la lease, la richiesta si
// annulla e il vecchio host riprende".
func TestCancelDuringReservationLetsOldHostResume(t *testing.T) {
	c, _ := newTest()
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.RequestHandoff("B", "pcB")
	_ = c.Release("A", "pcA", a.Generation, "B")

	if _, err := c.Acquire("A", "pcA"); !errors.Is(err, ErrReserved) {
		t.Fatalf("durante la riserva anche A doveva essere fermo: %v", err)
	}
	if err := c.CancelHandoff("C", "pcC"); !errors.Is(err, ErrNoHandoff) {
		t.Fatalf("C non ha una riserva: atteso ErrNoHandoff, ottenuto %v", err)
	}
	if err := c.CancelHandoff("B", "pcB"); err != nil {
		t.Fatal(err)
	}
	l, err := c.Acquire("A", "pcA")
	if err != nil || l.Generation != 2 {
		t.Fatalf("A doveva riprendere subito la lease: %+v, %v", l, err)
	}
}

func TestExpiredReservationCannotBeCancelled(t *testing.T) {
	c, clk := newTest()
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.RequestHandoff("B", "pcB")
	_ = c.Release("A", "pcA", a.Generation, "B")
	clk.Advance(HandoffReservation + time.Second)
	if err := c.CancelHandoff("B", "pcB"); !errors.Is(err, ErrNoHandoff) {
		t.Fatalf("riserva scaduta: atteso ErrNoHandoff, ottenuto %v", err)
	}
}

func TestHandoffDiesWithLease(t *testing.T) {
	c, clk := newTest()
	_, _ = c.Acquire("A", "pcA")
	_, _ = c.RequestHandoff("B", "pcB")

	clk.Advance(LeaseTTL + time.Second)
	if _, ok := c.PendingHandoff(); ok {
		t.Fatal("con la lease scaduta la richiesta non conta più")
	}
	// qualcuno prende la lease libera: la vecchia richiesta non deve sopravvivere
	if _, err := c.Acquire("C", "pcC"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.PendingHandoff(); ok {
		t.Fatal("la richiesta di B apparteneva alla lease di A")
	}
	h, err := c.RequestHandoff("D", "pcD")
	if err != nil || h.From != "C" || h.To != "D" {
		t.Fatalf("D doveva poter chiedere a C: %+v, %v", h, err)
	}
}

func TestHandoffIsPersisted(t *testing.T) {
	c, clk, s := newPersistentTest(t)
	_, _ = c.Acquire("A", "pcA")
	before := s.saves
	h, err := c.RequestHandoff("B", "pcB")
	if err != nil {
		t.Fatal(err)
	}
	if s.saves != before+1 {
		t.Fatalf("salvataggi: %d, ne attendevo %d", s.saves, before+1)
	}
	if s.last.Handoff == nil || *s.last.Handoff != h {
		t.Fatalf("la richiesta non è nello stato salvato: %+v", s.last.Handoff)
	}
	// dopo un riavvio la richiesta c'è ancora
	st := s.last
	c2, err := NewPersistent(clk, nil, &fakeSaver{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c2.PendingHandoff()
	if !ok || got != h {
		t.Fatalf("dopo il riavvio: %+v, %v", got, ok)
	}
}

func TestSaveFailureRollsBackHandoff(t *testing.T) {
	c, _, s := newPersistentTest(t)
	_, _ = c.Acquire("A", "pcA")

	s.fail = errors.New("disco pieno")
	if _, err := c.RequestHandoff("B", "pcB"); !errors.Is(err, ErrPersist) {
		t.Fatalf("atteso ErrPersist, ottenuto %v", err)
	}
	if _, ok := c.PendingHandoff(); ok {
		t.Fatal("la richiesta non doveva esistere")
	}

	s.fail = nil
	if _, err := c.RequestHandoff("B", "pcB"); err != nil {
		t.Fatal(err)
	}
	s.fail = errors.New("disco pieno")
	if err := c.CancelHandoff("B", "pcB"); !errors.Is(err, ErrPersist) {
		t.Fatalf("atteso ErrPersist, ottenuto %v", err)
	}
	if _, ok := c.PendingHandoff(); !ok {
		t.Fatal("la richiesta doveva essere ancora lì")
	}
}

func TestSaveFailureOnReleaseKeepsHandoff(t *testing.T) {
	c, _, s := newPersistentTest(t)
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.RequestHandoff("B", "pcB")

	s.fail = errors.New("disco pieno")
	if err := c.Release("A", "pcA", a.Generation, "B"); !errors.Is(err, ErrPersist) {
		t.Fatalf("atteso ErrPersist, ottenuto %v", err)
	}
	if _, ok := c.PendingHandoff(); !ok {
		t.Fatal("rilascio non salvato: la richiesta doveva restare")
	}
}

func TestRestoreRejectsBadHandoff(t *testing.T) {
	good := func() State {
		return State{
			Format: StateFormat, Generation: 1,
			Lease:    &Lease{Player: "A", Device: "pcA", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)},
			Handoff:  &HandoffRequest{From: "A", To: "B", ToDevice: "pcB"},
			Versions: []Version{}, Orphans: []OrphanState{},
		}
	}
	if _, err := NewPersistent(&fakeClock{t: time.Now()}, nil, &fakeSaver{}, ptr(good())); err != nil {
		t.Fatalf("lo stato buono doveva caricarsi: %v", err)
	}
	cases := map[string]func(*State){
		"senza lease":        func(s *State) { s.Lease = nil },
		"lease di un altro":  func(s *State) { s.Handoff.From = "X" },
		"senza destinatario": func(s *State) { s.Handoff.To = "" },
		"senza dispositivo":  func(s *State) { s.Handoff.ToDevice = "" },
	}
	for name, mutate := range cases {
		st := good()
		mutate(&st)
		if _, err := NewPersistent(&fakeClock{t: time.Now()}, nil, &fakeSaver{}, &st); !errors.Is(err, ErrBadState) {
			t.Errorf("%s: atteso ErrBadState, ottenuto %v", name, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }
