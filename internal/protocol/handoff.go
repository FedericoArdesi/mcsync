package protocol

import (
	"errors"
	"time"
)

// Passaggio di host (6.7): un giocatore chiede di diventare host mentre un
// altro ospita. L'host attuale vede la richiesta, salva, si ferma, carica e
// rilascia con handoffTo. La richiesta vive finché vive la lease dell'host:
// se la lease scade o viene rilasciata, la richiesta non ha più senso.

var (
	ErrAlreadyHost    = errors.New("il giocatore è già l'host")
	ErrHandoffPending = errors.New("c'è già una richiesta di passaggio in corso")
	ErrNoHandoff      = errors.New("nessuna richiesta di passaggio da annullare")
	ErrNotRequester   = errors.New("la richiesta di passaggio è di un altro giocatore o dispositivo")
)

// HandoffRequest è la richiesta di passaggio di host in corso.
type HandoffRequest struct {
	// From è l'host al momento della richiesta.
	From string `json:"from"`
	// To e ToDevice sono il giocatore e il dispositivo che chiedono di ospitare.
	To       string    `json:"to"`
	ToDevice string    `json:"to_device"`
	At       time.Time `json:"at"`
}

// RequestHandoff scrive una richiesta di passaggio verso (player, device).
//
// Serve una lease in corso di un altro giocatore: se chi chiede è già l'host
// (anche con un altro dispositivo) la richiesta è rifiutata. Ne esiste una
// sola alla volta: se ce n'è già un'altra, chi arriva dopo riceve
// ErrHandoffPending e dovrà entrare come client. Ripetere la stessa richiesta
// (stesso giocatore e dispositivo) non è un errore e restituisce quella
// esistente, così un nuovo tentativo dopo un'interruzione di rete è sicuro.
func (c *Coordinator) RequestHandoff(player, device string) (HandoffRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()

	if c.lease == nil {
		return HandoffRequest{}, ErrNoLease
	}
	if !now.Before(c.lease.ExpiresAt) {
		return HandoffRequest{}, ErrLeaseExpired
	}
	if c.lease.Player == player {
		return HandoffRequest{}, ErrAlreadyHost
	}
	if h := c.handoff; h != nil {
		if h.To == player && h.ToDevice == device {
			return *h, nil
		}
		return HandoffRequest{}, ErrHandoffPending
	}

	u := c.markLocked()
	h := HandoffRequest{From: c.lease.Player, To: player, ToDevice: device, At: now}
	c.handoff = &h
	if err := c.persistLocked(u); err != nil {
		return HandoffRequest{}, err
	}
	return h, nil
}

// PendingHandoff restituisce la richiesta di passaggio in corso, se la lease
// dell'host è ancora valida. L'host la legge a ogni heartbeat.
func (c *Coordinator) PendingHandoff() (HandoffRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handoff == nil || c.lease == nil || !c.clock.Now().Before(c.lease.ExpiresAt) {
		return HandoffRequest{}, false
	}
	return *c.handoff, true
}

// CancelHandoff ritira la richiesta di (player, device). Vale in due momenti:
//   - prima che l'host rilasci: la richiesta sparisce e l'host continua;
//   - dopo il rilascio, mentre la lease è riservata a player: la riserva
//     sparisce, così il vecchio host (la cui copia è ancora la versione
//     ufficiale) può riprendere subito la lease senza aspettare i 3 minuti.
//
// Solo chi ha fatto la richiesta può ritirarla.
func (c *Coordinator) CancelHandoff(player, device string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if h := c.handoff; h != nil {
		if h.To != player || h.ToDevice != device {
			return ErrNotRequester
		}
		u := c.markLocked()
		c.handoff = nil
		return c.persistLocked(u)
	}
	if r := c.reservation; r != nil && r.For == player && c.clock.Now().Before(r.Until) {
		u := c.markLocked()
		c.reservation = nil
		return c.persistLocked(u)
	}
	return ErrNoHandoff
}
