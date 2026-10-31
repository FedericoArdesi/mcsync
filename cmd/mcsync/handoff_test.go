package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FedericoArdesi/mcsync/internal/api"
	"github.com/FedericoArdesi/mcsync/internal/client"
)

// fakeHandoffAPI risponde a Status con una sequenza decisa dal test (l'ultima
// risposta si ripete) e conta richieste e ritiri.
type fakeHandoffAPI struct {
	statuses []api.Status
	reqErr   error
	onStatus func(n int) // chiamata a ogni Status, n da 1

	statusCalls  int
	requests     int
	cancels      int
	cancelCtxErr error // ctx.Err() visto da CancelHandoff
}

func (f *fakeHandoffAPI) Status(ctx context.Context) (api.Status, error) {
	if err := ctx.Err(); err != nil {
		return api.Status{}, err
	}
	f.statusCalls++
	if f.onStatus != nil {
		f.onStatus(f.statusCalls)
	}
	i := f.statusCalls - 1
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	return f.statuses[i], nil
}

func (f *fakeHandoffAPI) RequestHandoff(context.Context) (api.HandoffRequest, error) {
	f.requests++
	if f.reqErr != nil {
		return api.HandoffRequest{}, f.reqErr
	}
	return api.HandoffRequest{From: "Fede", To: "Ale", ToDevice: "dev-ale"}, nil
}

func (f *fakeHandoffAPI) CancelHandoff(ctx context.Context) error {
	f.cancels++
	f.cancelCtxErr = ctx.Err()
	return nil
}

var me = api.Identity{Player: "Ale", Device: "dev-ale"}

func freeStatus() api.Status { return api.Status{You: me} }

func hostStatus() api.Status {
	return api.Status{You: me, Lease: &api.Lease{Player: "Fede", Generation: 1, ExpiresInSeconds: 300}}
}

func pendingStatus() api.Status {
	s := hostStatus()
	s.Handoff = &api.HandoffRequest{From: "Fede", To: "Ale", ToDevice: "dev-ale"}
	return s
}

func reservedStatus(who string) api.Status {
	return api.Status{You: me, Reservation: &api.Reservation{For: who, ExpiresInSeconds: 170}}
}

func wait(f *fakeHandoffAPI, ctx context.Context, timeout time.Duration) (string, error) {
	var out strings.Builder
	err := waitForHandoff(ctx, f, timeout, 2*time.Millisecond, &out)
	return out.String(), err
}

func TestWaitForHandoffSucceedsWhenReservedForMe(t *testing.T) {
	f := &fakeHandoffAPI{statuses: []api.Status{hostStatus(), pendingStatus(), pendingStatus(), reservedStatus("Ale")}}
	out, err := wait(f, context.Background(), 2*time.Second)
	if err != nil {
		t.Fatalf("errore = %v", err)
	}
	if f.requests != 1 || f.cancels != 0 {
		t.Fatalf("richieste %d, ritiri %d (attesi 1 e 0)", f.requests, f.cancels)
	}
	if !strings.Contains(out, "Fede") {
		t.Errorf("il messaggio deve nominare l'host: %q", out)
	}
}

func TestWaitForHandoffNobodyPlayingSkipsRequest(t *testing.T) {
	f := &fakeHandoffAPI{statuses: []api.Status{freeStatus()}}
	if _, err := wait(f, context.Background(), time.Second); err != nil {
		t.Fatalf("errore = %v", err)
	}
	if f.requests != 0 {
		t.Fatalf("richieste = %d: se nessuno gioca non si chiede niente", f.requests)
	}
}

func TestWaitForHandoffAlreadyReservedForMeSkipsRequest(t *testing.T) {
	f := &fakeHandoffAPI{statuses: []api.Status{reservedStatus("Ale")}}
	if _, err := wait(f, context.Background(), time.Second); err != nil {
		t.Fatalf("errore = %v", err)
	}
	if f.requests != 0 {
		t.Fatalf("richieste = %d", f.requests)
	}
}

func TestWaitForHandoffRefusesWhenAlreadyHost(t *testing.T) {
	s := hostStatus()
	s.Lease.Player = "Ale"
	f := &fakeHandoffAPI{statuses: []api.Status{s}}
	_, err := wait(f, context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "già host") {
		t.Fatalf("errore = %v", err)
	}
	if f.requests != 0 {
		t.Fatalf("richieste = %d", f.requests)
	}
}

func TestWaitForHandoffRefusesWhenReservedForSomeoneElse(t *testing.T) {
	f := &fakeHandoffAPI{statuses: []api.Status{reservedStatus("Gio")}}
	_, err := wait(f, context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "Gio") {
		t.Fatalf("errore = %v", err)
	}
	if f.requests != 0 {
		t.Fatalf("richieste = %d", f.requests)
	}
}

func TestWaitForHandoffExplainsPendingRequestOfSomeoneElse(t *testing.T) {
	f := &fakeHandoffAPI{
		statuses: []api.Status{hostStatus()},
		reqErr:   &client.APIError{ErrorBody: api.ErrorBody{Code: api.CodeHandoffPending}, HTTPStatus: 409},
	}
	_, err := wait(f, context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "già una richiesta") {
		t.Fatalf("errore = %v", err)
	}
	if f.cancels != 0 {
		t.Fatalf("ritiri = %d: non si ritira una richiesta che non è nostra", f.cancels)
	}
}

func TestWaitForHandoffTimeoutWithdrawsRequest(t *testing.T) {
	f := &fakeHandoffAPI{statuses: []api.Status{hostStatus(), pendingStatus()}}
	_, err := wait(f, context.Background(), 60*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "non ha risposto") {
		t.Fatalf("errore = %v", err)
	}
	if f.cancels != 1 {
		t.Fatalf("ritiri = %d, atteso 1", f.cancels)
	}
}

func TestWaitForHandoffCtrlCWithdrawsRequestWithLiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeHandoffAPI{statuses: []api.Status{hostStatus(), pendingStatus()}}
	f.onStatus = func(n int) {
		if n == 3 {
			cancel() // "Ctrl+C"
		}
	}
	_, err := wait(f, ctx, 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errore = %v, atteso context.Canceled", err)
	}
	if f.cancels != 1 {
		t.Fatalf("ritiri = %d, atteso 1", f.cancels)
	}
	if f.cancelCtxErr != nil {
		t.Fatalf("il ritiro è partito con un contesto già annullato (%v): non arriverebbe mai al servizio", f.cancelCtxErr)
	}
}

func TestWaitForHandoffRequestVanished(t *testing.T) {
	// l'host è ancora lo stesso ma la richiesta non c'è più e non c'è riserva
	f := &fakeHandoffAPI{statuses: []api.Status{hostStatus(), pendingStatus(), hostStatus()}}
	_, err := wait(f, context.Background(), 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "non c'è più") {
		t.Fatalf("errore = %v", err)
	}
	if f.cancels != 0 {
		t.Fatalf("ritiri = %d", f.cancels)
	}
}

func TestWaitForHandoffHostGoneLetsRunProceed(t *testing.T) {
	// la lease dell'host è scaduta o è stata rilasciata senza riserva: nessuno
	// gioca più, si può provare a prenderla
	f := &fakeHandoffAPI{statuses: []api.Status{hostStatus(), pendingStatus(), freeStatus()}}
	if _, err := wait(f, context.Background(), 2*time.Second); err != nil {
		t.Fatalf("errore = %v", err)
	}
	if f.cancels != 0 {
		t.Fatalf("ritiri = %d", f.cancels)
	}
}
