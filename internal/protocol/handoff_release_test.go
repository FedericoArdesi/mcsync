package protocol

import (
	"errors"
	"testing"
)

// Seconda difesa (6.7): handoff_to riserva la lease solo se coincide con la
// richiesta di passaggio in corso. In tutti gli altri casi la lease viene
// rilasciata lo stesso, ma senza riserva: un rilascio non deve mai fallire
// per colpa di una richiesta cambiata mentre l'host chiudeva.

func TestReleaseWithoutRequestIgnoresHandoffTo(t *testing.T) {
	c, _ := newTest()
	a, _ := c.Acquire("A", "pcA")
	if err := c.Release("A", "pcA", a.Generation, "B"); err != nil {
		t.Fatalf("il rilascio doveva riuscire comunque: %v", err)
	}
	if _, err := c.Acquire("C", "pcC"); err != nil {
		t.Fatalf("nessuna richiesta, nessuna riserva: C doveva entrare, ottenuto %v", err)
	}
}

func TestReleaseToSomeoneElseThanRequesterIgnoresHandoffTo(t *testing.T) {
	c, _ := newTest()
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.RequestHandoff("C", "pcC")
	if err := c.Release("A", "pcA", a.Generation, "B"); err != nil {
		t.Fatalf("il rilascio doveva riuscire comunque: %v", err)
	}
	// B non ha chiesto nulla: nessuna riserva per B, e nemmeno per C
	if _, err := c.Acquire("D", "pcD"); err != nil {
		t.Fatalf("nessuna riserva: D doveva entrare, ottenuto %v", err)
	}
}

func TestReleaseAfterRequestCancelledIgnoresHandoffTo(t *testing.T) {
	c, _ := newTest()
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.RequestHandoff("B", "pcB")
	// B ritira mentre A sta ancora chiudendo, A non lo sa e rilascia a B
	if err := c.CancelHandoff("B", "pcB"); err != nil {
		t.Fatal(err)
	}
	if err := c.Release("A", "pcA", a.Generation, "B"); err != nil {
		t.Fatalf("il rilascio doveva riuscire comunque: %v", err)
	}
	if _, err := c.Acquire("A", "pcA"); err != nil {
		t.Fatalf("richiesta ritirata: A doveva poter riprendere subito, ottenuto %v", err)
	}
}

func TestReleaseToRequesterReserves(t *testing.T) {
	c, _ := newTest()
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.RequestHandoff("B", "pcB")
	if err := c.Release("A", "pcA", a.Generation, "B"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire("C", "pcC"); !errors.Is(err, ErrReserved) {
		t.Fatalf("C: atteso ErrReserved, ottenuto %v", err)
	}
}

// Una richiesta in corso non basta da sola: senza handoff_to la lease si
// rilascia libera (è la scelta dell'host, che oggi riguarda lo stato prima di
// rilasciare).
func TestReleaseWithoutHandoffToDoesNotReserveForPendingRequest(t *testing.T) {
	c, _ := newTest()
	a, _ := c.Acquire("A", "pcA")
	_, _ = c.RequestHandoff("B", "pcB")
	if err := c.Release("A", "pcA", a.Generation, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire("C", "pcC"); err != nil {
		t.Fatalf("senza handoff_to nessuna riserva: C doveva entrare, ottenuto %v", err)
	}
}
