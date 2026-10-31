package server

import (
	"testing"
	"time"

	"github.com/FedericoArdesi/mcsync/internal/api"
	"github.com/FedericoArdesi/mcsync/internal/protocol"
)

func (c *client) requestHandoff() (api.HandoffRequest, int, api.ErrorBody) {
	c.t.Helper()
	var h api.HandoffRequest
	code, eb := c.call("POST", api.PathHandoffRequest, nil, &h)
	return h, code, eb
}

func (c *client) cancelHandoff() (int, api.ErrorBody) {
	c.t.Helper()
	return c.call("POST", api.PathHandoffCancel, nil, nil)
}

func TestHandoffRequestIsSeenByHost(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	gio := e.enroll("Gio")
	l := fede.mustAcquire()

	if hb, code, _ := fede.heartbeat(l.Generation); code != 200 || hb.Handoff != nil {
		t.Fatalf("senza richieste il heartbeat non porta niente: %d %+v", code, hb)
	}
	h, code, eb := ale.requestHandoff()
	if code != 200 || h.From != "Fede" || h.To != "Ale" || h.ToDevice != ale.device {
		t.Fatalf("richiesta di Ale: %d %v %+v", code, eb, h)
	}
	// l'host la vede al heartbeat e la sua lease resta valida
	hb, code, eb := fede.heartbeat(l.Generation)
	if code != 200 || hb.Handoff == nil || hb.Handoff.To != "Ale" {
		t.Fatalf("heartbeat di Fede: %d %v %+v", code, eb, hb)
	}
	// chiunque la vede anche nello stato
	st, _, _ := gio.status()
	if st.Handoff == nil || st.Handoff.To != "Ale" || st.Handoff.From != "Fede" {
		t.Fatalf("stato: %+v", st.Handoff)
	}
	// una sola alla volta; la stessa ripetuta non è un errore
	if _, code, eb := gio.requestHandoff(); code != 409 || eb.Code != api.CodeHandoffPending {
		t.Fatalf("Gio: atteso 409 handoff_pending, ottenuto %d %v", code, eb)
	}
	if again, code, _ := ale.requestHandoff(); code != 200 || again != h {
		t.Fatalf("ripetizione: %d %+v", code, again)
	}
	// l'host non può chiedere a sé stesso
	if _, code, eb := fede.requestHandoff(); code != 409 || eb.Code != api.CodeAlreadyHost {
		t.Fatalf("Fede: atteso 409 already_host, ottenuto %d %v", code, eb)
	}
}

func TestHandoffRequestNeedsLiveLease(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	if _, code, eb := ale.requestHandoff(); code != 409 || eb.Code != api.CodeNoLease {
		t.Fatalf("senza lease: atteso 409 no_lease, ottenuto %d %v", code, eb)
	}
	fede.mustAcquire()
	e.clk.Advance(protocol.LeaseTTL + time.Second)
	if _, code, eb := ale.requestHandoff(); code != 409 || eb.Code != api.CodeLeaseExpired {
		t.Fatalf("lease scaduta: atteso 409 lease_expired, ottenuto %d %v", code, eb)
	}
}

func TestHandoffCancel(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	gio := e.enroll("Gio")
	l := fede.mustAcquire()
	ale.mustRequestHandoff()

	if code, eb := gio.cancelHandoff(); code != 403 || eb.Code != api.CodeNotRequester {
		t.Fatalf("Gio: atteso 403 not_requester, ottenuto %d %v", code, eb)
	}
	if code, eb := ale.cancelHandoff(); code != 204 {
		t.Fatalf("Ale: %d %v", code, eb)
	}
	if hb, code, _ := fede.heartbeat(l.Generation); code != 200 || hb.Handoff != nil {
		t.Fatalf("dopo il ritiro l'host continua senza richieste: %d %+v", code, hb)
	}
	if code, eb := ale.cancelHandoff(); code != 409 || eb.Code != api.CodeNoHandoff {
		t.Fatalf("secondo ritiro: atteso 409 no_handoff, ottenuto %d %v", code, eb)
	}
	if _, code, eb := gio.requestHandoff(); code != 200 {
		t.Fatalf("ora Gio può chiedere: %d %v", code, eb)
	}
}

func TestHandoffFullFlowOverHTTP(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	gio := e.enroll("Gio")
	l := fede.mustAcquire()
	ale.mustRequestHandoff()

	if code, eb := fede.release(l.Generation, "Ale"); code != 204 {
		t.Fatalf("rilascio verso Ale: %d %v", code, eb)
	}
	st, _, _ := gio.status()
	if st.Handoff != nil || st.Reservation == nil || st.Reservation.For != "Ale" {
		t.Fatalf("dopo il rilascio: niente richiesta, riserva per Ale: %+v %+v", st.Handoff, st.Reservation)
	}
	if _, code, eb := gio.acquire(); code != 409 || eb.Code != api.CodeLeaseReserved {
		t.Fatalf("Gio: atteso 409 lease_reserved, ottenuto %d %v", code, eb)
	}
	if l2 := ale.mustAcquire(); l2.Generation != 2 {
		t.Fatalf("Ale doveva avere la generazione 2: %+v", l2)
	}
}

func TestHandoffCancelDuringReservationLetsOldHostResume(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	l := fede.mustAcquire()
	ale.mustRequestHandoff()
	if code, eb := fede.release(l.Generation, "Ale"); code != 204 {
		t.Fatalf("rilascio verso Ale: %d %v", code, eb)
	}

	if code, eb := ale.cancelHandoff(); code != 204 {
		t.Fatalf("ritiro durante la riserva: %d %v", code, eb)
	}
	if l2, code, eb := fede.acquire(); code != 200 || l2.Generation != 2 {
		t.Fatalf("Fede doveva riprendere subito: %d %v %+v", code, eb, l2)
	}
}

func TestHandoffSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	l := fede.mustAcquire()
	ale.mustRequestHandoff()

	e.restart()

	hb, code, eb := fede.heartbeat(l.Generation)
	if code != 200 || hb.Handoff == nil || hb.Handoff.To != "Ale" {
		t.Fatalf("la richiesta doveva sopravvivere al riavvio: %d %v %+v", code, eb, hb)
	}
}

func TestHandoffNeedsToken(t *testing.T) {
	e := newEnv(t)
	anon := e.anon()
	for _, p := range []string{api.PathHandoffRequest, api.PathHandoffCancel} {
		if code, eb := anon.call("POST", p, nil, nil); code != 401 || eb.Code != api.CodeUnauthorized {
			t.Fatalf("%s senza token: atteso 401 unauthorized, ottenuto %d %v", p, code, eb)
		}
	}
}

func (c *client) mustRequestHandoff() {
	c.t.Helper()
	if _, code, eb := c.requestHandoff(); code != 200 {
		c.t.Fatalf("%s doveva poter chiedere il passaggio: %d %v", c.player, code, eb)
	}
}

// Seconda difesa (6.7): handoff_to riserva la lease solo al giocatore che ha
// davvero una richiesta in corso. Se la richiesta è stata ritirata mentre
// l'host chiudeva, il rilascio riesce lo stesso (204) ma la lease resta libera.
func TestReleaseWithWithdrawnRequestDoesNotReserve(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	gio := e.enroll("Gio")
	l := fede.mustAcquire()

	if _, code, eb := ale.requestHandoff(); code != 200 {
		t.Fatalf("richiesta di Ale: %d %v", code, eb)
	}
	if code, eb := ale.cancelHandoff(); code != 204 {
		t.Fatalf("ritiro di Ale: %d %v", code, eb)
	}
	// Fede non lo sa ancora e rilascia ad Ale
	if code, eb := fede.release(l.Generation, "Ale"); code != 204 {
		t.Fatalf("il rilascio doveva riuscire comunque: %d %v", code, eb)
	}
	st, _, _ := gio.status()
	if st.Lease != nil || st.Reservation != nil {
		t.Fatalf("nessuna riserva attesa: %+v %+v", st.Lease, st.Reservation)
	}
	if _, code, eb := gio.acquire(); code != 200 {
		t.Fatalf("Gio doveva poter prendere la lease subito: %d %v", code, eb)
	}
}

// Un handoff_to diverso dal richiedente non dirotta la riserva.
func TestReleaseToNonRequesterDoesNotReserve(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	gio := e.enroll("Gio")
	l := fede.mustAcquire()

	if _, code, eb := ale.requestHandoff(); code != 200 {
		t.Fatalf("richiesta di Ale: %d %v", code, eb)
	}
	if code, eb := fede.release(l.Generation, "Gio"); code != 204 {
		t.Fatalf("il rilascio doveva riuscire comunque: %d %v", code, eb)
	}
	st, _, _ := ale.status()
	if st.Reservation != nil {
		t.Fatalf("nessuna riserva attesa (né per Gio né per Ale): %+v", st.Reservation)
	}
	if _, code, eb := gio.acquire(); code != 200 {
		t.Fatalf("Gio doveva poter prendere la lease: %d %v", code, eb)
	}
}
