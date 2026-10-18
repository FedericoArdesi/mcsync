// Package client è il lato client dell'API di rete (internal/api): quello che
// l'agente sul PC usa per parlare con il servizio sul laptop.
//
// Non contiene decisioni: ogni metodo fa una richiesta e traduce la risposta.
// Gli errori del servizio arrivano come *APIError, con il codice fisso di
// api.Code*; tutto il resto (rete caduta, flusso rovinato) è un errore normale.
// Il token non finisce mai in un messaggio d'errore.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mcsync/internal/api"
	"mcsync/internal/tarutil"
)

// APIError è un errore risposto dal servizio.
type APIError struct {
	api.ErrorBody
	// HTTPStatus è il codice HTTP della risposta.
	HTTPStatus int
	// RetryAfter è il tempo suggerito dal servizio (503), se c'è.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("%s (%d): %s", e.Code, e.HTTPStatus, e.Message)
	if e.RequestID != "" {
		s += " [richiesta " + e.RequestID + "]"
	}
	return s
}

// IsCode dice se err è un errore del servizio con uno dei codici indicati.
func IsCode(err error, codes ...string) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	for _, c := range codes {
		if ae.Code == c {
			return true
		}
	}
	return false
}

// Client parla con un servizio. Si usa da più goroutine insieme.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New crea un client per il servizio all'indirizzo addr (ip:porta) con il
// token del dispositivo.
func New(addr, token string) *Client {
	return &Client{base: baseURL(addr), token: token, hc: newHTTPClient()}
}

// NewWithHTTP è come New ma con un client HTTP dato (per i test).
func NewWithHTTP(base, token string, hc *http.Client) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, hc: hc}
}

func baseURL(addr string) string {
	if strings.Contains(addr, "://") {
		return strings.TrimRight(addr, "/")
	}
	return "http://" + addr
}

// newHTTPClient non ha una scadenza totale (un trasferimento grande può durare
// minuti): chi chiama usa il contesto. I controlli di rete morta sono nel
// dialer (keep-alive).
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}).DialContext
	tr.Proxy = nil // il servizio è su una rete privata: niente proxy
	return &http.Client{Transport: tr}
}

// Enroll iscrive un dispositivo nuovo con un codice di invito (8.3) e
// restituisce il suo token, che si vede una volta sola. È l'unica richiesta
// senza token.
func Enroll(ctx context.Context, invite, label string) (addr string, resp api.EnrollResponse, err error) {
	addr, secret, err := api.DecodeInviteCode(invite)
	if err != nil {
		return "", resp, err
	}
	c := New(addr, "")
	err = c.doJSON(ctx, http.MethodPost, api.PathEnroll, api.EnrollRequest{Invite: secret, Label: label}, &resp)
	return addr, resp, err
}

func (c *Client) Status(ctx context.Context) (api.Status, error) {
	var st api.Status
	err := c.doJSON(ctx, http.MethodGet, api.PathStatus, nil, &st)
	return st, err
}

func (c *Client) Acquire(ctx context.Context) (api.Lease, error) {
	var l api.Lease
	err := c.doJSON(ctx, http.MethodPost, api.PathAcquire, struct{}{}, &l)
	return l, err
}

func (c *Client) Heartbeat(ctx context.Context, generation uint64) (api.Lease, error) {
	var l api.Lease
	err := c.doJSON(ctx, http.MethodPost, api.PathHeartbeat, api.HeartbeatRequest{Generation: generation}, &l)
	return l, err
}

func (c *Client) Release(ctx context.Context, generation uint64, handoffTo string) error {
	return c.doJSON(ctx, http.MethodPost, api.PathRelease, api.ReleaseRequest{Generation: generation, HandoffTo: handoffTo}, nil)
}

// Upload manda il contenuto di dir al servizio e chiede il commit. Il mondo
// viaggia come flusso tar con sigillo (tarutil.Pack) senza essere prima
// scritto su disco. Se la lettura di dir fallisce a metà, il flusso resta
// senza sigillo e il servizio lo rifiuta: non nasce mai una versione a metà.
//
// dir deve essere ferma per tutta la durata: a chi chiama il compito di
// passare una copia (il mirror), non la cartella viva del gioco.
func (c *Client) Upload(ctx context.Context, generation, parent uint64, dir string, skip func(rel string) bool) (api.UploadResponse, error) {
	pr, pw := io.Pipe()
	packErr := make(chan error, 1)
	go func() {
		err := tarutil.Pack(pw, dir, skip)
		packErr <- err
		pw.CloseWithError(err) // con err nil chiude normalmente
	}()
	defer pr.Close() // se la richiesta finisce prima, Pack si ferma

	q := url.Values{}
	q.Set(api.QueryGeneration, strconv.FormatUint(generation, 10))
	q.Set(api.QueryParent, strconv.FormatUint(parent, 10))
	req, err := c.newRequest(ctx, http.MethodPost, api.PathUpload+"?"+q.Encode(), pr)
	if err != nil {
		return api.UploadResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-tar")

	var out api.UploadResponse
	err = c.do(req, &out)
	if err != nil {
		// se la causa è la lettura della cartella, è quella l'errore utile
		select {
		case pe := <-packErr:
			if pe != nil && !IsCode(err) {
				return out, fmt.Errorf("lettura del mondo: %w", pe)
			}
		default:
		}
		return out, err
	}
	return out, nil
}

// Download scarica una versione (0 = la corrente) e la scrive in dst, che deve
// essere vuota o non esistere. Se restituisce un errore dst può contenere file
// a metà: chi chiama la cancella. Restituisce il numero della versione
// scaricata. Un flusso interrotto o rovinato è rifiutato dal sigillo.
func (c *Client) Download(ctx context.Context, version uint64, dst string, lim tarutil.Limits) (uint64, error) {
	path := api.PathDownload
	if version != 0 {
		path += "?" + api.QueryVersion + "=" + strconv.FormatUint(version, 10)
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, c.clean(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, readAPIError(resp)
	}
	got, err := strconv.ParseUint(resp.Header.Get(api.HeaderVersion), 10, 64)
	if err != nil || got == 0 {
		return 0, errors.New("il servizio non ha indicato la versione scaricata")
	}
	if version != 0 && got != version {
		return 0, fmt.Errorf("il servizio ha mandato la versione %d invece della %d", got, version)
	}
	if err := tarutil.Unpack(resp.Body, dst, lim); err != nil {
		return 0, fmt.Errorf("download della versione %d: %w", got, err)
	}
	return got, nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(api.HeaderProtocol, strconv.Itoa(api.ProtocolVersion))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(b))
	}
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.clean(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return readAPIError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("risposta del servizio non leggibile: %w", err)
	}
	return nil
}

// clean toglie l'indirizzo completo dagli errori di rete (url.Error): contiene
// solo il percorso e nessun segreto, ma il messaggio resta più corto.
func (c *Client) clean(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %w", ue.Op, strings.TrimPrefix(ue.URL, c.base), ue.Err)
	}
	return err
}

func readAPIError(resp *http.Response) error {
	ae := &APIError{HTTPStatus: resp.StatusCode}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		ae.RetryAfter = time.Duration(s) * time.Second
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(raw, &ae.ErrorBody); err != nil || ae.Code == "" {
		ae.Code = api.CodeInternal
		ae.Message = "risposta inattesa dal servizio: " + http.StatusText(resp.StatusCode)
	}
	if ae.RequestID == "" {
		ae.RequestID = resp.Header.Get(api.HeaderRequestID)
	}
	return ae
}
