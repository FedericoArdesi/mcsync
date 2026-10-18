// Package admin è l'amministrazione del servizio (inviti, elenco e revoca dei
// dispositivi). Non passa dalla rete: il servizio in funzione la offre solo su
// un socket Unix dentro la sua cartella dati, leggibile solo dall'utente che lo
// ha avviato (8.2: "l'amministrazione si fa sul laptop").
//
// Serve perché il servizio tiene i dispositivi in memoria: un comando che
// scrivesse il file da un altro processo non verrebbe visto, e il servizio al
// salvataggio successivo lo sovrascriverebbe.
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"mcsync/internal/auth"
)

const (
	codeBadPlayer     = "bad_player"
	codeUnknownDevice = "unknown_device"
	maxBody           = 16 << 10
)

type inviteReq struct {
	Player string `json:"player"`
}
type inviteResp struct {
	Secret string `json:"secret"`
}
type revokeReq struct {
	DeviceID string `json:"device_id"`
}
type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Handler è l'amministrazione sopra l'archivio dei dispositivi del servizio.
func Handler(a *auth.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /invite", func(w http.ResponseWriter, r *http.Request) {
		var req inviteReq
		if !readJSON(w, r, &req) {
			return
		}
		secret, err := a.CreateInvite(req.Player)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, inviteResp{Secret: secret})
	})
	mux.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, a.Devices())
	})
	mux.HandleFunc("POST /revoke", func(w http.ResponseWriter, r *http.Request) {
		var req revokeReq
		if !readJSON(w, r, &req) {
			return
		}
		if err := a.Revoke(req.DeviceID); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil {
		writeJSON(w, 400, errBody{Code: "bad_request", Message: "corpo non valido"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrBadPlayer):
		writeJSON(w, 400, errBody{Code: codeBadPlayer, Message: err.Error()})
	case errors.Is(err, auth.ErrUnknownDevice):
		writeJSON(w, 404, errBody{Code: codeUnknownDevice, Message: err.Error()})
	default:
		writeJSON(w, 500, errBody{Code: "internal", Message: "errore interno"})
	}
}

// Serve offre h sul socket Unix path finché ctx non viene annullato. Un socket
// rimasto da un servizio morto viene tolto; se qualcuno risponde, c'è già un
// servizio in funzione e Serve si rifiuta di partire.
func Serve(ctx context.Context, path string, h http.Handler) error {
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		_ = c.Close()
		return fmt.Errorf("c'è già un servizio in funzione (socket %s)", path)
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		<-errc
		_ = os.Remove(path)
		return nil
	}
}

// Client parla con l'amministrazione di un servizio in funzione.
type Client struct {
	hc *http.Client
}

// ErrNotRunning: nessun servizio risponde sul socket.
var ErrNotRunning = errors.New("nessun servizio in funzione")

// NewClient crea un client per il socket path.
func NewClient(path string) *Client {
	return &Client{hc: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}}
}

// Running dice se un servizio risponde sul socket.
func Running(path string) bool {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func (c *Client) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://admin"+path, body)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var eb errBody
		_ = json.NewDecoder(resp.Body).Decode(&eb)
		switch eb.Code {
		case codeBadPlayer:
			return auth.ErrBadPlayer
		case codeUnknownDevice:
			return auth.ErrUnknownDevice
		}
		return fmt.Errorf("il servizio ha risposto %d: %s", resp.StatusCode, eb.Message)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Invite crea un invito per un giocatore e restituisce il segreto.
func (c *Client) Invite(player string) (string, error) {
	var out inviteResp
	if err := c.do("POST", "/invite", inviteReq{Player: player}, &out); err != nil {
		return "", err
	}
	return out.Secret, nil
}

// Devices elenca i dispositivi (senza gli hash dei token).
func (c *Client) Devices() ([]auth.Device, error) {
	var out []auth.Device
	err := c.do("GET", "/devices", nil, &out)
	return out, err
}

// Revoke revoca un dispositivo.
func (c *Client) Revoke(deviceID string) error {
	return c.do("POST", "/revoke", revokeReq{DeviceID: deviceID}, nil)
}
