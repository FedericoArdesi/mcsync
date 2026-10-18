package protocol

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time          { return f.t }
func (f *fakeClock) Advance(d time.Duration) { f.t = f.t.Add(d) }

func newTest() (*Coordinator, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	return NewCoordinator(clk, nil), clk
}

func commitReq(l Lease, parent uint64) CommitRequest {
	return CommitRequest{
		Player: l.Player, Device: l.Device, Generation: l.Generation,
		Parent: parent, BlocksVerified: true, SizeBytes: 1,
	}
}

func TestAcquireIncrementsGeneration(t *testing.T) {
	c, clk := newTest()
	a, err := c.Acquire("A", "pcA")
	if err != nil || a.Generation != 1 {
		t.Fatalf("prima lease: %+v, %v", a, err)
	}
	if err := c.Release("A", "pcA", a.Generation, ""); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	b, err := c.Acquire("B", "pcB")
	if err != nil || b.Generation != 2 {
		t.Fatalf("seconda lease: %+v, %v", b, err)
	}
}

func TestSimultaneousAcquireOneWinner(t *testing.T) {
	c, _ := newTest()
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := c.Acquire(fmt.Sprintf("p%d", i), fmt.Sprintf("dev%d", i))
			if err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrHeld) {
				t.Errorf("errore inatteso: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("vincitori = %d, atteso 1", wins)
	}
}

func TestSameDeviceReacquireRenews(t *testing.T) {
	c, clk := newTest()
	a, _ := c.Acquire("A", "pcA")
	clk.Advance(5 * time.Minute)
	a2, err := c.Acquire("A", "pcA")
	if err != nil || a2.Generation != a.Generation {
		t.Fatalf("atteso rinnovo con stessa generazione: %+v, %v", a2, err)
	}
	if !a2.ExpiresAt.After(a.ExpiresAt) {
		t.Fatal("la scadenza non è stata spostata avanti")
	}
}

func TestSecondDeviceSamePlayerBlocked(t *testing.T) {
	c, _ := newTest()
	if _, err := c.Acquire("A", "pc1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire("A", "pc2"); !errors.Is(err, ErrHeld) {
		t.Fatalf("atteso ErrHeld, ottenuto %v", err)
	}
}

func TestHeartbeatKeepsLeaseAlive(t *testing.T) {
	c, clk := newTest()
	a, _ := c.Acquire("A", "pcA")
	clk.Advance(9 * time.Minute)
	if _, err := c.Heartbeat("A", "pcA", a.Generation); err != nil {
		t.Fatal(err)
	}
	clk.Advance(9 * time.Minute) // 18 min totali, ma l'ultimo heartbeat ne ha 9
	if _, err := c.Acquire("B", "pcB"); !errors.Is(err, ErrHeld) {
		t.Fatalf("la lease doveva essere ancora di A, ottenuto %v", err)
	}
}

// Sezione 7: "Vecchio host che si risveglia in ritardo".
func TestLateOldHostIsRejected(t *testing.T) {
	c, clk := newTest()
	a, _ := c.Acquire("A", "pcA")
	clk.Advance(LeaseTTL + time.Second)
	b, err := c.Acquire("B", "pcB")
	if err != nil || b.Generation != 2 {
		t.Fatalf("B doveva prendere la lease con generazione 2: %+v, %v", b, err)
	}
	if _, err := c.Commit(commitReq(a, 0)); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("commit di A: atteso ErrStaleGeneration, ottenuto %v", err)
	}
	if _, err := c.Heartbeat("A", "pcA", a.Generation); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("heartbeat di A: atteso ErrStaleGeneration, ottenuto %v", err)
	}
	if got := len(c.Orphans()); got != 1 {
		t.Fatalf("orfani = %d, atteso 1", got)
	}
	if c.Pointer() != 0 {
		t.Fatal("il puntatore non doveva muoversi")
	}
}

func TestExpiredLeaseCannotCommit(t *testing.T) {
	c, clk := newTest()
	a, _ := c.Acquire("A", "pcA")
	clk.Advance(LeaseTTL)
	if _, err := c.Commit(commitReq(a, 0)); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("atteso ErrLeaseExpired, ottenuto %v", err)
	}
}

// Sezione 7: "L'upload di A finisce dopo che B ha già confermato".
func TestCommitCompareAndSwap(t *testing.T) {
	c, _ := newTest()
	a, _ := c.Acquire("A", "pcA")
	v1, err := c.Commit(commitReq(a, 0))
	if err != nil || v1.Number != 1 || c.Pointer() != 1 {
		t.Fatalf("primo commit: %+v, %v", v1, err)
	}
	// un secondo upload ancora basato sulla versione 0 è in ritardo
	if _, err := c.Commit(commitReq(a, 0)); !errors.Is(err, ErrParentMismatch) {
		t.Fatalf("atteso ErrParentMismatch, ottenuto %v", err)
	}
	if c.Pointer() != 1 || len(c.Orphans()) != 1 {
		t.Fatal("puntatore o orfani non corretti")
	}
	// uno basato sulla versione corrente passa
	if v2, err := c.Commit(commitReq(a, 1)); err != nil || v2.Number != 2 || v2.Parent != 1 {
		t.Fatalf("secondo commit: %+v, %v", v2, err)
	}
}

func TestIncompleteBlocksAreNotOrphans(t *testing.T) {
	c, _ := newTest()
	a, _ := c.Acquire("A", "pcA")
	req := commitReq(a, 0)
	req.BlocksVerified = false
	if _, err := c.Commit(req); !errors.Is(err, ErrBlocksIncomplete) {
		t.Fatalf("atteso ErrBlocksIncomplete, ottenuto %v", err)
	}
	if len(c.Orphans()) != 0 || c.Pointer() != 0 {
		t.Fatal("un upload incompleto riprende: niente orfano, niente versione")
	}
}

func TestNoSpace(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	c := NewCoordinator(clk, func() int64 { return 100 })
	a, _ := c.Acquire("A", "pcA")
	req := commitReq(a, 0)
	req.SizeBytes = 101
	if _, err := c.Commit(req); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("atteso ErrNoSpace, ottenuto %v", err)
	}
	req.SizeBytes = 100
	if _, err := c.Commit(req); err != nil {
		t.Fatalf("con spazio sufficiente doveva passare: %v", err)
	}
}

// Sezione 7: "Un terzo si infila durante un passaggio di host".
func TestHandoffReservationBlocksThirdPlayer(t *testing.T) {
	c, clk := newTest()
	a, _ := c.Acquire("A", "pcA")
	if err := c.Release("A", "pcA", a.Generation, "B"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire("C", "pcC"); !errors.Is(err, ErrReserved) {
		t.Fatalf("C: atteso ErrReserved, ottenuto %v", err)
	}
	clk.Advance(time.Minute)
	b, err := c.Acquire("B", "pcB")
	if err != nil || b.Generation != 2 {
		t.Fatalf("B doveva prendere la lease riservata: %+v, %v", b, err)
	}
}

// Sezione 7: "Il nuovo host chiude prima di prendere la lease".
func TestHandoffReservationExpires(t *testing.T) {
	c, clk := newTest()
	a, _ := c.Acquire("A", "pcA")
	_ = c.Release("A", "pcA", a.Generation, "B")
	clk.Advance(HandoffReservation + time.Second)
	if _, err := c.Acquire("C", "pcC"); err != nil {
		t.Fatalf("la riserva è scaduta, C doveva poter entrare: %v", err)
	}
}

func TestReleaseWithStaleGenerationRejected(t *testing.T) {
	c, clk := newTest()
	a, _ := c.Acquire("A", "pcA")
	clk.Advance(LeaseTTL + time.Second)
	_, _ = c.Acquire("B", "pcB")
	if err := c.Release("A", "pcA", a.Generation, ""); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("atteso ErrStaleGeneration, ottenuto %v", err)
	}
	if _, ok := c.CurrentLease(); !ok {
		t.Fatal("la lease di B doveva restare intatta")
	}
}
