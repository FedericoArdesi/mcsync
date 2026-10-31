package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"mcsync/internal/api"
)

// handoffRemote avvolge fakeRemote: dal heartbeat numero afterHB in poi
// riporta una richiesta di passaggio, e ricorda a chi è stata rilasciata la
// lease e quanti upload c'erano in quel momento.
type handoffRemote struct {
	*fakeRemote
	afterHB int
	handoff *api.HandoffRequest

	mu               sync.Mutex
	releaseCalls     int
	releasedTo       string
	uploadsAtRelease int
}

func newHandoffRemote(afterHB int) *handoffRemote {
	return &handoffRemote{
		fakeRemote: newFake(),
		afterHB:    afterHB,
		handoff:    &api.HandoffRequest{From: "Fede", To: "Ale", ToDevice: "dev-ale"},
	}
}

func (r *handoffRemote) Heartbeat(ctx context.Context, gen uint64) (api.Lease, error) {
	l, err := r.fakeRemote.Heartbeat(ctx, gen)
	if err != nil {
		return l, err
	}
	r.fakeRemote.mu.Lock()
	n := r.fakeRemote.heartbeats
	r.fakeRemote.mu.Unlock()
	if r.afterHB > 0 && n >= r.afterHB {
		l.Handoff = r.handoff
	}
	return l, nil
}

func (r *handoffRemote) Release(ctx context.Context, gen uint64, to string) error {
	r.fakeRemote.mu.Lock()
	uploads := r.fakeRemote.uploads
	r.fakeRemote.mu.Unlock()
	r.mu.Lock()
	r.releaseCalls++
	r.releasedTo = to
	r.uploadsAtRelease = uploads
	r.mu.Unlock()
	return r.fakeRemote.Release(ctx, gen, to)
}

func TestHandoffRequestMakesHostSaveThenReleaseToRequester(t *testing.T) {
	r := newHandoffRemote(3)
	o := fastOpts(t)
	o.UploadEvery = time.Hour // l'unico upload deve essere quello finale

	start := time.Now()
	err := runFor(t, r, o, 3*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Run doveva chiudersi da solo alla richiesta, non per il Ctrl+C")
	}
	if r.releaseCalls != 1 || r.releasedTo != "Ale" {
		t.Fatalf("rilascio: %d chiamate, handoff_to = %q (atteso una, \"Ale\")", r.releaseCalls, r.releasedTo)
	}
	if r.fakeRemote.uploads != 1 || r.uploadsAtRelease != 1 {
		t.Fatalf("il salvataggio finale doveva precedere il rilascio: upload %d, al rilascio %d", r.fakeRemote.uploads, r.uploadsAtRelease)
	}
	st, err := ReadState(o.workDir())
	if err != nil {
		t.Fatal(err)
	}
	if st.Dirty || st.BaseVersion != r.fakeRemote.pointer {
		t.Errorf("stato locale = %+v, attesa base %d e non dirty", st, r.fakeRemote.pointer)
	}
}

func TestHandoffWithFailedFinalUploadDoesNotRelease(t *testing.T) {
	r := newHandoffRemote(3)
	r.fakeRemote.uploadErr = func(int) error { return errors.New("rete caduta") }
	o := fastOpts(t)
	o.UploadEvery = time.Hour

	start := time.Now()
	err := runFor(t, r, o, 3*time.Second)
	if !errors.Is(err, ErrNotSaved) {
		t.Fatalf("errore = %v, atteso ErrNotSaved", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Run doveva fermarsi da solo alla richiesta, non per il Ctrl+C")
	}
	if r.releaseCalls != 0 {
		t.Fatalf("senza salvataggio non si passa l'host: %d rilasci verso %q", r.releaseCalls, r.releasedTo)
	}
}

func TestNoHandoffRequestReleasesWithoutTarget(t *testing.T) {
	r := newHandoffRemote(0) // il heartbeat non porta mai richieste
	o := fastOpts(t)
	if err := runFor(t, r, o, 150*time.Millisecond); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.releaseCalls != 1 || r.releasedTo != "" {
		t.Fatalf("rilascio: %d chiamate, handoff_to = %q (atteso una, vuoto)", r.releaseCalls, r.releasedTo)
	}
}
