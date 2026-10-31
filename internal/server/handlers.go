package server

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/FedericoArdesi/mcsync/internal/api"
	"github.com/FedericoArdesi/mcsync/internal/auth"
	"github.com/FedericoArdesi/mcsync/internal/protocol"
)

// enroll registra un dispositivo nuovo con un codice di invito (8.3). È l'unica
// richiesta senza token. Un codice sbagliato, scaduto o già usato dà sempre la
// stessa risposta.
func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var req api.EnrollRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	dev, token, err := s.auth.Redeem(req.Invite, req.Label)
	if errors.Is(err, auth.ErrBadInvite) {
		s.log.Warn("iscrizione rifiutata", "id", infoOf(r.Context()).id)
		s.failMsg(w, r, http.StatusForbidden, api.CodeBadInvite, auth.ErrBadInvite.Error(), "")
		return
	}
	if err != nil {
		s.fail(w, r, err, "")
		return
	}
	s.log.Info("dispositivo registrato", slog.String("device", dev.ID), slog.String("player", dev.Player), slog.String("label", dev.Label))
	writeJSON(w, http.StatusOK, api.EnrollResponse{DeviceID: dev.ID, Player: dev.Player, Token: token})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, dev auth.Device) {
	now := s.clock.Now()
	st := api.Status{
		Protocol:   api.ProtocolVersion,
		ServerTime: now,
		You:        api.Identity{Player: dev.Player, Device: dev.ID},
		Pointer:    s.coord.Pointer(),
	}
	if v, ok := s.coord.Version(st.Pointer); ok {
		dto := versionDTO(v)
		st.Current = &dto
	}
	if l, ok := s.coord.CurrentLease(); ok {
		dto := leaseDTO(l, now)
		st.Lease = &dto
	}
	if res := s.coord.State().Reservation; res != nil && now.Before(res.Until) {
		st.Reservation = &api.Reservation{For: res.For, Until: res.Until, ExpiresInSeconds: secondsUntil(res.Until, now)}
	}
	if hr, ok := s.coord.PendingHandoff(); ok {
		dto := handoffDTO(hr)
		st.Handoff = &dto
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) acquire(w http.ResponseWriter, r *http.Request, dev auth.Device) {
	l, err := s.coord.Acquire(dev.Player, dev.ID)
	if err != nil {
		s.fail(w, r, err, "")
		return
	}
	s.log.Info("lease presa", "player", dev.Player, "device", dev.ID, "generation", l.Generation)
	writeJSON(w, http.StatusOK, leaseDTO(l, s.clock.Now()))
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, dev auth.Device) {
	var req api.HeartbeatRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	l, err := s.coord.Heartbeat(dev.Player, dev.ID, req.Generation)
	if err != nil {
		s.fail(w, r, err, "")
		return
	}
	dto := leaseDTO(l, s.clock.Now())
	if hr, ok := s.coord.PendingHandoff(); ok {
		hd := handoffDTO(hr)
		dto.Handoff = &hd
	}
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) release(w http.ResponseWriter, r *http.Request, dev auth.Device) {
	var req api.ReleaseRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	// il nome arriva da fuori: si controlla che sia valido e che sia un
	// giocatore che esiste (8.2, "nomi validati contro players.json")
	if req.HandoffTo != "" && (!auth.ValidPlayer(req.HandoffTo) || !s.auth.HasPlayer(req.HandoffTo)) {
		s.failMsg(w, r, http.StatusBadRequest, api.CodeBadRequest, "handoff_to non è un giocatore registrato", "")
		return
	}
	if err := s.coord.Release(dev.Player, dev.ID, req.Generation, req.HandoffTo); err != nil {
		s.fail(w, r, err, "")
		return
	}
	s.log.Info("lease rilasciata", "player", dev.Player, "device", dev.ID, "generation", req.Generation, "handoff_to", req.HandoffTo)
	w.WriteHeader(http.StatusNoContent)
}

func leaseDTO(l protocol.Lease, now time.Time) api.Lease {
	return api.Lease{
		Player: l.Player, Device: l.Device, Generation: l.Generation,
		ExpiresAt: l.ExpiresAt, ExpiresInSeconds: secondsUntil(l.ExpiresAt, now),
	}
}

func versionDTO(v protocol.Version) api.Version {
	return api.Version{
		Number: v.Number, Parent: v.Parent, Author: v.Author, Device: v.Device,
		Generation: v.Generation, CreatedAt: v.CreatedAt,
	}
}

func secondsUntil(t, now time.Time) int {
	if s := int(t.Sub(now).Seconds()); s > 0 {
		return s
	}
	return 0
}

// handoffRequest: il giocatore del token chiede di diventare host (6.7).
// Chi chiede si ricava dal token; la richiesta non ha corpo.
func (s *Server) handoffRequest(w http.ResponseWriter, r *http.Request, dev auth.Device) {
	h, err := s.coord.RequestHandoff(dev.Player, dev.ID)
	if err != nil {
		s.fail(w, r, err, "")
		return
	}
	s.log.Info("passaggio di host richiesto", "from", h.From, "to", dev.Player, "device", dev.ID)
	writeJSON(w, http.StatusOK, handoffDTO(h))
}

// handoffCancel: chi ha fatto la richiesta la ritira, prima o dopo il
// rilascio dell'host.
func (s *Server) handoffCancel(w http.ResponseWriter, r *http.Request, dev auth.Device) {
	if err := s.coord.CancelHandoff(dev.Player, dev.ID); err != nil {
		s.fail(w, r, err, "")
		return
	}
	s.log.Info("passaggio di host ritirato", "player", dev.Player, "device", dev.ID)
	w.WriteHeader(http.StatusNoContent)
}

func handoffDTO(h protocol.HandoffRequest) api.HandoffRequest {
	return api.HandoffRequest{From: h.From, To: h.To, ToDevice: h.ToDevice, At: h.At}
}
