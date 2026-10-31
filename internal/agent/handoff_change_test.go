package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FedericoArdesi/mcsync/internal/api"
)

// changingRemote simula la richiesta che cambia mentre l'host salva: dopo il
// primo upload il heartbeat riporta replacement (nil = richiesta ritirata).
type changingRemote struct {
	*handoffRemote
	changed     atomic.Bool
	replacement *api.HandoffRequest
}

func (r *changingRemote) Heartbeat(ctx context.Context, gen uint64) (api.Lease, error) {
	l, err := r.handoffRemote.Heartbeat(ctx, gen)
	if err == nil && r.changed.Load() {
		l.Handoff = r.replacement
	}
	return l, err
}

func (r *changingRemote) Upload(ctx context.Context, gen, parent uint64, dir string, skip func(string) bool) (api.UploadResponse, error) {
	resp, err := r.handoffRemote.Upload(ctx, gen, parent, dir, skip)
	r.changed.Store(true)
	return resp, err
}

func TestHandoffWithdrawnWhileSavingReleasesWithoutTarget(t *testing.T) {
	r := &changingRemote{handoffRemote: newHandoffRemote(3)} // Ale ritira mentre Fede salva
	o := fastOpts(t)
	o.UploadEvery = time.Hour

	start := time.Now()
	if err := runFor(t, r, o, 3*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Run doveva chiudersi da solo alla richiesta, non per il Ctrl+C")
	}
	if r.releaseCalls != 1 || r.releasedTo != "" {
		t.Fatalf("rilascio: %d chiamate, handoff_to = %q (atteso una, vuoto: nessuno aspetta più)", r.releaseCalls, r.releasedTo)
	}
}

func TestHandoffTakenOverByAnotherRequesterWhileSaving(t *testing.T) {
	// Ale ritira e Gio chiede mentre Fede salva: la lease va a chi aspetta ora
	r := &changingRemote{
		handoffRemote: newHandoffRemote(3),
		replacement:   &api.HandoffRequest{From: "Fede", To: "Gio", ToDevice: "dev-gio"},
	}
	o := fastOpts(t)
	o.UploadEvery = time.Hour

	if err := runFor(t, r, o, 3*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.releaseCalls != 1 || r.releasedTo != "Gio" {
		t.Fatalf("rilascio: %d chiamate, handoff_to = %q (atteso una, \"Gio\")", r.releaseCalls, r.releasedTo)
	}
}
