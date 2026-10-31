package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/FedericoArdesi/mcsync/internal/api"
)

// cancelAfterCommit simula il Ctrl+C che arriva mentre un upload è in volo:
// il servizio conferma il commit, ma la risposta non torna all'agente.
type cancelAfterCommit struct {
	*fakeRemote
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelAfterCommit) Upload(ctx context.Context, gen, parent uint64, dir string, skip func(string) bool) (api.UploadResponse, error) {
	resp, err := c.fakeRemote.Upload(ctx, gen, parent, dir, skip)
	if err != nil {
		return resp, err
	}
	lost := false
	c.once.Do(func() { lost = true })
	if lost {
		c.cancel()
		return api.UploadResponse{}, context.Canceled
	}
	return resp, nil
}

func TestCtrlCDuringUploadAfterCommit(t *testing.T) {
	f := newFake()
	o := fastOpts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := &cancelAfterCommit{fakeRemote: f, cancel: cancel}
	if err := Run(ctx, r, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !f.released {
		t.Fatal("la lease doveva essere rilasciata")
	}
}
