package server

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcsync/internal/api"
	"mcsync/internal/archive"
	"mcsync/internal/auth"
	"mcsync/internal/protocol"
	"mcsync/internal/service"
	"mcsync/internal/statefile"
	"mcsync/internal/tarutil"
	"mcsync/internal/testutil"
)

// ---------------------------------------------------------------------------
// Ambiente di prova: servizio vero (stato su disco, archivio finto in memoria)
// dietro un server HTTP vero su localhost, con orologio finto.

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type env struct {
	t     *testing.T
	clk   *fakeClock
	mem   *archive.MemStore
	store archive.Store // quello che il servizio usa (di solito mem)
	logs  *syncBuffer
	cfg   func(*Config)
	saver protocol.Saver // se non è nil, il coordinatore usa questo invece di statefile

	root        string
	work        string
	statePath   string
	devicesPath string

	coord *protocol.Coordinator
	auth  *auth.Store
	srv   *Server
	ts    *httptest.Server

	clients []*client // per aggiornare l'indirizzo dopo un riavvio
	secrets []string  // token e codici di invito emessi, per controllare che non finiscano nei log
}

func newEnv(t *testing.T, opts ...func(*env)) *env {
	t.Helper()
	root := t.TempDir()
	mem := archive.NewMemStore()
	e := &env{
		t:           t,
		clk:         &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)},
		mem:         mem,
		store:       mem,
		logs:        &syncBuffer{},
		root:        root,
		work:        filepath.Join(root, "work"),
		statePath:   filepath.Join(root, "state", "state.json"),
		devicesPath: filepath.Join(root, "state", "devices.json"),
	}
	for _, o := range opts {
		o(e)
	}
	e.start()
	return e
}

// start (ri)crea coordinatore, archivio dei dispositivi, server e listener
// partendo da quello che c'è su disco: serve anche a simulare un riavvio.
func (e *env) start() {
	e.t.Helper()
	var err error
	if e.saver != nil {
		e.coord, err = protocol.NewPersistent(e.clk, nil, e.saver, nil)
	} else {
		e.coord, err = statefile.Open(e.statePath, e.clk, nil)
	}
	if err != nil {
		e.t.Fatal(err)
	}
	e.auth, err = auth.Open(e.devicesPath, e.clk)
	if err != nil {
		e.t.Fatal(err)
	}
	cfg := Config{
		Coordinator: e.coord,
		Service:     service.New(e.coord, e.store),
		Auth:        e.auth,
		Clock:       e.clk,
		WorkDir:     e.work,
		Logger:      slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if e.cfg != nil {
		e.cfg(&cfg)
	}
	e.srv, err = New(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	e.ts = httptest.NewServer(e.srv.Handler())
	e.t.Cleanup(e.ts.Close)
}

func (e *env) restart() {
	e.t.Helper()
	e.ts.Close()
	e.start()
	for _, c := range e.clients {
		c.base = e.ts.URL
	}
}

// client è un client finto: parla HTTP con il server, niente di più.
type client struct {
	t      *testing.T
	e      *env
	base   string
	token  string
	player string
	device string
	hc     *http.Client
}

func (e *env) anon() *client {
	return &client{t: e.t, e: e, base: e.ts.URL, hc: e.ts.Client()}
}

// enroll crea un invito, lo usa tramite la rete e restituisce un client con
// il token del dispositivo nuovo.
func (e *env) enroll(player string) *client {
	e.t.Helper()
	secret, err := e.auth.CreateInvite(player)
	if err != nil {
		e.t.Fatal(err)
	}
	e.secrets = append(e.secrets, secret)
	c := e.anon()
	var out api.EnrollResponse
	if st, eb := c.call("POST", api.PathEnroll, api.EnrollRequest{Invite: secret, Label: "PC di " + player}, &out); st != 200 {
		e.t.Fatalf("iscrizione di %s: %d %v", player, st, eb)
	}
	e.secrets = append(e.secrets, out.Token)
	c.token, c.player, c.device = out.Token, out.Player, out.DeviceID
	e.clients = append(e.clients, c)
	return c
}

func (c *client) tryDo(method, path string, body io.Reader, mods ...func(*http.Request)) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set(api.HeaderProtocol, strconv.Itoa(api.ProtocolVersion))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for _, m := range mods {
		m(req)
	}
	return c.hc.Do(req)
}

func (c *client) do(method, path string, body io.Reader, mods ...func(*http.Request)) *http.Response {
	c.t.Helper()
	resp, err := c.tryDo(method, path, body, mods...)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// call manda un JSON e legge la risposta: out se va bene, ErrorBody se no.
func (c *client) call(method, path string, in, out any) (int, api.ErrorBody) {
	c.t.Helper()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			c.t.Fatal(err)
		}
		body = bytes.NewReader(b)
	}
	resp := c.do(method, path, body)
	defer resp.Body.Close()
	var eb api.ErrorBody
	if resp.StatusCode/100 == 2 {
		if out != nil && resp.StatusCode != http.StatusNoContent {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				c.t.Fatalf("risposta non leggibile: %v", err)
			}
		}
		return resp.StatusCode, eb
	}
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	return resp.StatusCode, eb
}

func (c *client) status() (api.Status, int, api.ErrorBody) {
	c.t.Helper()
	var st api.Status
	code, eb := c.call("GET", api.PathStatus, nil, &st)
	return st, code, eb
}

func (c *client) acquire() (api.Lease, int, api.ErrorBody) {
	c.t.Helper()
	var l api.Lease
	code, eb := c.call("POST", api.PathAcquire, nil, &l)
	return l, code, eb
}

func (c *client) mustAcquire() api.Lease {
	c.t.Helper()
	l, code, eb := c.acquire()
	if code != 200 {
		c.t.Fatalf("%s doveva prendere la lease: %d %v", c.player, code, eb)
	}
	return l
}

func (c *client) heartbeat(gen uint64) (api.Lease, int, api.ErrorBody) {
	c.t.Helper()
	var l api.Lease
	code, eb := c.call("POST", api.PathHeartbeat, api.HeartbeatRequest{Generation: gen}, &l)
	return l, code, eb
}

func (c *client) release(gen uint64, handoffTo string) (int, api.ErrorBody) {
	c.t.Helper()
	return c.call("POST", api.PathRelease, api.ReleaseRequest{Generation: gen, HandoffTo: handoffTo}, nil)
}

func (c *client) uploadURL(gen, parent uint64) string {
	return fmt.Sprintf("%s?%s=%d&%s=%d", api.PathUpload, api.QueryGeneration, gen, api.QueryParent, parent)
}

// upload manda body come corpo dell'upload e legge la risposta.
func (c *client) upload(gen, parent uint64, body io.Reader) (int, api.UploadResponse, api.ErrorBody) {
	c.t.Helper()
	resp := c.do("POST", c.uploadURL(gen, parent), body)
	defer resp.Body.Close()
	var ok api.UploadResponse
	var eb api.ErrorBody
	if resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(&ok); err != nil {
			c.t.Fatal(err)
		}
	} else {
		_ = json.NewDecoder(resp.Body).Decode(&eb)
	}
	return resp.StatusCode, ok, eb
}

func (c *client) uploadWorld(gen, parent uint64, files map[string]string) (int, api.UploadResponse, api.ErrorBody) {
	c.t.Helper()
	return c.upload(gen, parent, bytes.NewReader(packTree(c.t, files)))
}

func (c *client) mustUpload(gen, parent uint64, files map[string]string) api.UploadResponse {
	c.t.Helper()
	code, ok, eb := c.uploadWorld(gen, parent, files)
	if code != 200 {
		c.t.Fatalf("upload di %s: %d %v", c.player, code, eb)
	}
	return ok
}

// download scarica e disfa il flusso. Se la risposta è un errore, files è nil.
func (c *client) download(query string) (code int, files map[string]string, hdr http.Header, eb api.ErrorBody) {
	c.t.Helper()
	resp := c.do("GET", api.PathDownload+query, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		_ = json.NewDecoder(resp.Body).Decode(&eb)
		return resp.StatusCode, nil, resp.Header, eb
	}
	dst := filepath.Join(c.t.TempDir(), "dl")
	if err := tarutil.Unpack(resp.Body, dst, tarutil.Limits{}); err != nil {
		c.t.Fatalf("il flusso scaricato non è valido: %v", err)
	}
	return 200, testutil.ReadTree(c.t, dst), resp.Header, eb
}

// ---------------------------------------------------------------------------
// Mondi e flussi di prova

var testWorld = map[string]string{
	"level.dat":        "livello",
	"region/r.0.0.mca": strings.Repeat("R", 3000),
	"players/a.dat":    "giocatore",
	"session.lock":     "non deve arrivare nell'archivio",
	"logs/latest.log":  "nemmeno questo",
}

// stored è quello che l'archivio deve contenere per testWorld.
var stored = map[string]string{
	"level.dat":        "livello",
	"region/r.0.0.mca": strings.Repeat("R", 3000),
	"players/a.dat":    "giocatore",
}

func packTree(t *testing.T, files map[string]string) []byte {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	testutil.WriteTree(t, dir, files)
	var buf bytes.Buffer
	if err := tarutil.Pack(&buf, dir, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// sealed aggiunge a un tar il sigillo finale, come fa tarutil.Pack.
func sealed(tarBytes []byte) []byte {
	sum := sha256.Sum256(tarBytes)
	out := append([]byte(nil), tarBytes...)
	out = append(out, "mcsync-tar1\n"...)
	return append(out, sum[:]...)
}

func rawTar(t *testing.T, hdrs ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bytes.Repeat([]byte("d"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("non è successo entro 5 secondi: %s", what)
}

func (e *env) tmpEntries() []string {
	entries, _ := os.ReadDir(filepath.Join(e.work, "tmp"))
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	return names
}

func (e *env) snapshots() []archive.SnapshotID {
	ids, err := e.mem.List(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return ids
}

// ---------------------------------------------------------------------------
// Iscrizione, token, protocollo

func TestEnrollThenStatus(t *testing.T) {
	e := newEnv(t)
	c := e.enroll("Fede")
	st, code, eb := c.status()
	if code != 200 {
		t.Fatalf("status: %d %v", code, eb)
	}
	if st.You.Player != "Fede" || st.You.Device != c.device || st.Protocol != api.ProtocolVersion {
		t.Fatalf("identità o protocollo inattesi: %+v", st)
	}
	if st.Pointer != 0 || st.Current != nil || st.Lease != nil || st.Reservation != nil {
		t.Fatalf("lo stato doveva essere vuoto: %+v", st)
	}
	if !st.ServerTime.Equal(e.clk.Now()) {
		t.Fatalf("server_time = %v, atteso l'orologio del laptop %v", st.ServerTime, e.clk.Now())
	}
	resp := c.do("GET", api.PathStatus, nil)
	resp.Body.Close()
	if resp.Header.Get(api.HeaderRequestID) == "" {
		t.Fatal("ogni risposta deve avere un identificativo di richiesta")
	}
}

func TestEnrollRejectsBadInvites(t *testing.T) {
	e := newEnv(t)
	anon := e.anon()

	secret, _ := e.auth.CreateInvite("Fede")
	req := api.EnrollRequest{Invite: secret, Label: "PC"}
	if code, eb := anon.call("POST", api.PathEnroll, req, &api.EnrollResponse{}); code != 200 {
		t.Fatalf("primo uso: %d %v", code, eb)
	}
	if code, eb := anon.call("POST", api.PathEnroll, req, nil); code != 403 || eb.Code != api.CodeBadInvite {
		t.Fatalf("secondo uso: atteso 403 bad_invite, ottenuto %d %v", code, eb)
	}

	expired, _ := e.auth.CreateInvite("Ale")
	e.clk.Advance(auth.InviteTTL)
	if code, eb := anon.call("POST", api.PathEnroll, api.EnrollRequest{Invite: expired}, nil); code != 403 || eb.Code != api.CodeBadInvite {
		t.Fatalf("invito scaduto: atteso 403 bad_invite, ottenuto %d %v", code, eb)
	}
	if code, eb := anon.call("POST", api.PathEnroll, api.EnrollRequest{Invite: "inv_inventato"}, nil); code != 403 || eb.Code != api.CodeBadInvite {
		t.Fatalf("invito inventato: atteso 403 bad_invite, ottenuto %d %v", code, eb)
	}

	resp := anon.do("POST", api.PathEnroll, strings.NewReader("{non è json"))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("corpo non valido: atteso 400, ottenuto %d", resp.StatusCode)
	}
	if n := len(e.auth.Devices()); n != 1 {
		t.Fatalf("doveva esistere un solo dispositivo, ce ne sono %d", n)
	}
}

func TestTokenRequiredEverywhere(t *testing.T) {
	e := newEnv(t)
	good := e.enroll("Fede")
	endpoints := []struct{ method, path string }{
		{"GET", api.PathStatus},
		{"POST", api.PathAcquire},
		{"POST", api.PathHeartbeat},
		{"POST", api.PathRelease},
		{"POST", good.uploadURL(1, 0)},
		{"GET", api.PathDownload},
	}
	badAuth := map[string]func(*http.Request){
		"senza intestazione": func(r *http.Request) { r.Header.Del("Authorization") },
		"token inventato":    func(r *http.Request) { r.Header.Set("Authorization", "Bearer mcs_inventato") },
		"schema sbagliato":   func(r *http.Request) { r.Header.Set("Authorization", "Basic "+good.token) },
		"solo Bearer":        func(r *http.Request) { r.Header.Set("Authorization", "Bearer") },
		"token con spazi":    func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+good.token+" extra") },
		"token vuoto":        func(r *http.Request) { r.Header.Set("Authorization", "Bearer  ") },
	}
	anon := e.anon()
	for _, ep := range endpoints {
		for name, mod := range badAuth {
			resp := anon.do(ep.method, ep.path, strings.NewReader("{}"), mod)
			var eb api.ErrorBody
			_ = json.NewDecoder(resp.Body).Decode(&eb)
			resp.Body.Close()
			if resp.StatusCode != 401 || eb.Code != api.CodeUnauthorized {
				t.Fatalf("%s %s (%s): atteso 401 unauthorized, ottenuto %d %v", ep.method, ep.path, name, resp.StatusCode, eb)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Fatalf("%s %s (%s): manca WWW-Authenticate", ep.method, ep.path, name)
			}
		}
	}
	if len(e.snapshots()) != 0 || e.coord.Pointer() != 0 {
		t.Fatal("richieste senza token non devono toccare niente")
	}
}

func TestRevokedTokenStopsWorkingOnlyForThatDevice(t *testing.T) {
	e := newEnv(t)
	a := e.enroll("Fede")
	b := e.enroll("Ale")
	if _, code, _ := a.status(); code != 200 {
		t.Fatal("il dispositivo doveva funzionare prima della revoca")
	}
	if err := e.auth.Revoke(a.device); err != nil {
		t.Fatal(err)
	}
	if _, code, eb := a.status(); code != 401 || eb.Code != api.CodeUnauthorized {
		t.Fatalf("token revocato: atteso 401, ottenuto %d %v", code, eb)
	}
	if _, code, eb := a.acquire(); code != 401 {
		t.Fatalf("token revocato, acquire: atteso 401, ottenuto %d %v", code, eb)
	}
	if _, code, _ := b.status(); code != 200 {
		t.Fatal("l'altro dispositivo doveva continuare a funzionare")
	}
}

func TestRevokedHostCannotKeepHeartbeating(t *testing.T) {
	e := newEnv(t)
	a := e.enroll("Fede")
	l := a.mustAcquire()
	if err := e.auth.Revoke(a.device); err != nil {
		t.Fatal(err)
	}
	if _, code, _ := a.heartbeat(l.Generation); code != 401 {
		t.Fatalf("heartbeat con token revocato: atteso 401, ottenuto %d", code)
	}
	// la lease non viene più rinnovata e scade da sola: un altro può prenderla
	b := e.enroll("Ale")
	e.clk.Advance(protocol.LeaseTTL + time.Second)
	if l2, code, eb := b.acquire(); code != 200 || l2.Generation != l.Generation+1 {
		t.Fatalf("dopo la scadenza Ale doveva prendere la lease: %d %v %+v", code, eb, l2)
	}
}

func TestProtocolVersionIsChecked(t *testing.T) {
	e := newEnv(t)
	c := e.enroll("Fede")
	for name, mod := range map[string]func(*http.Request){
		"manca":       func(r *http.Request) { r.Header.Del(api.HeaderProtocol) },
		"troppo alta": func(r *http.Request) { r.Header.Set(api.HeaderProtocol, "2") },
		"zero":        func(r *http.Request) { r.Header.Set(api.HeaderProtocol, "0") },
		"non numero":  func(r *http.Request) { r.Header.Set(api.HeaderProtocol, "uno") },
	} {
		for _, path := range []string{api.PathStatus, api.PathEnroll} {
			method := "GET"
			if path == api.PathEnroll {
				method = "POST"
			}
			resp := c.do(method, path, strings.NewReader("{}"), mod)
			var eb api.ErrorBody
			_ = json.NewDecoder(resp.Body).Decode(&eb)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUpgradeRequired || eb.Code != api.CodeProtocol {
				t.Fatalf("%s %s: atteso 426 protocol_version, ottenuto %d %v", name, path, resp.StatusCode, eb)
			}
		}
	}
}

func TestUnknownPathAndWrongMethod(t *testing.T) {
	e := newEnv(t)
	c := e.enroll("Fede")
	resp := c.do("GET", "/v1/non-esiste", nil)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("percorso sconosciuto: atteso 404, ottenuto %d", resp.StatusCode)
	}
	resp = c.do("GET", api.PathUpload, nil)
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("metodo sbagliato: atteso 405, ottenuto %d", resp.StatusCode)
	}
	resp = c.do("DELETE", api.PathDownload, nil)
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("non deve esistere nessuna cancellazione: atteso 405, ottenuto %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Lease

func TestIdentityComesFromTokenNotFromBody(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	l := fede.mustAcquire()

	// Ale si finge Fede: nome e dispositivo nel corpo non contano
	body := fmt.Sprintf(`{"generation":%d,"player":"Fede","device":%q}`, l.Generation, fede.device)
	resp := ale.do("POST", api.PathHeartbeat, strings.NewReader(body))
	var eb api.ErrorBody
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	resp.Body.Close()
	if resp.StatusCode != 409 || eb.Code != api.CodeStaleGeneration {
		t.Fatalf("heartbeat di Ale: atteso 409 stale_generation, ottenuto %d %v", resp.StatusCode, eb)
	}
	if code, eb := ale.release(l.Generation, ""); code != 409 || eb.Code != api.CodeStaleGeneration {
		t.Fatalf("rilascio di Ale: atteso 409 stale_generation, ottenuto %d %v", code, eb)
	}
	if cur, ok := e.coord.CurrentLease(); !ok || cur.Player != "Fede" {
		t.Fatalf("la lease doveva restare di Fede: %+v %v", cur, ok)
	}
}

func TestLeaseFlow(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	gio := e.enroll("Gio")

	l := fede.mustAcquire()
	if l.Generation != 1 || l.Player != "Fede" || l.Device != fede.device || l.ExpiresInSeconds != int(protocol.LeaseTTL.Seconds()) {
		t.Fatalf("lease inattesa: %+v", l)
	}
	if _, code, eb := ale.acquire(); code != 409 || eb.Code != api.CodeLeaseHeld {
		t.Fatalf("Ale: atteso 409 lease_held, ottenuto %d %v", code, eb)
	}
	st, _, _ := ale.status()
	if st.Lease == nil || st.Lease.Player != "Fede" || st.Lease.Generation != 1 {
		t.Fatalf("Ale deve vedere la lease di Fede: %+v", st.Lease)
	}

	e.clk.Advance(5 * time.Minute)
	st, _, _ = ale.status()
	if st.Lease == nil || st.Lease.ExpiresInSeconds != 300 {
		t.Fatalf("dopo 5 minuti restano 300 secondi: %+v", st.Lease)
	}
	hb, code, eb := fede.heartbeat(l.Generation)
	if code != 200 || hb.ExpiresInSeconds != int(protocol.LeaseTTL.Seconds()) {
		t.Fatalf("heartbeat: %d %v %+v", code, eb, hb)
	}

	// passaggio di host: i nomi vengono controllati
	for _, bad := range []string{"Nessuno", "../x", "a b", "Fede\n"} {
		if code, eb := fede.release(l.Generation, bad); code != 400 || eb.Code != api.CodeBadRequest {
			t.Fatalf("handoff_to %q: atteso 400 bad_request, ottenuto %d %v", bad, code, eb)
		}
	}
	if cur, ok := e.coord.CurrentLease(); !ok || cur.Generation != 1 {
		t.Fatal("un rilascio rifiutato non deve toccare la lease")
	}
	if _, code, eb := ale.requestHandoff(); code != 200 {
		t.Fatalf("richiesta di passaggio di Ale: %d %v", code, eb)
	}
	if code, eb := fede.release(l.Generation, "Ale"); code != 204 {
		t.Fatalf("rilascio con passaggio ad Ale: %d %v", code, eb)
	}

	st, _, _ = gio.status()
	if st.Lease != nil || st.Reservation == nil || st.Reservation.For != "Ale" ||
		st.Reservation.ExpiresInSeconds != int(protocol.HandoffReservation.Seconds()) {
		t.Fatalf("deve esserci solo la riserva per Ale: %+v %+v", st.Lease, st.Reservation)
	}
	if _, code, eb := gio.acquire(); code != 409 || eb.Code != api.CodeLeaseReserved {
		t.Fatalf("Gio durante il passaggio: atteso 409 lease_reserved, ottenuto %d %v", code, eb)
	}
	l2 := ale.mustAcquire()
	if l2.Generation != 2 {
		t.Fatalf("Ale doveva avere la generazione 2: %+v", l2)
	}

	// la riserva scade dopo 3 minuti
	if _, code, eb := fede.requestHandoff(); code != 200 {
		t.Fatalf("richiesta di passaggio di Fede: %d %v", code, eb)
	}
	if code, eb := ale.release(l2.Generation, "Fede"); code != 204 {
		t.Fatalf("secondo passaggio: %d %v", code, eb)
	}
	e.clk.Advance(protocol.HandoffReservation + time.Second)
	st, _, _ = gio.status()
	if st.Reservation != nil {
		t.Fatalf("la riserva doveva essere scaduta: %+v", st.Reservation)
	}
	if l3, code, eb := gio.acquire(); code != 200 || l3.Generation != 3 {
		t.Fatalf("Gio dopo la scadenza della riserva: %d %v %+v", code, eb, l3)
	}

	// chi non ha più la lease non può rinnovarla né rilasciarla
	if _, code, eb := ale.heartbeat(l2.Generation); code != 409 || eb.Code != api.CodeStaleGeneration {
		t.Fatalf("heartbeat di una lease vecchia: atteso 409 stale_generation, ottenuto %d %v", code, eb)
	}
}

func TestHeartbeatAfterExpiryAndWithoutLease(t *testing.T) {
	e := newEnv(t)
	c := e.enroll("Fede")
	if _, code, eb := c.heartbeat(1); code != 409 || eb.Code != api.CodeNoLease {
		t.Fatalf("senza lease: atteso 409 no_lease, ottenuto %d %v", code, eb)
	}
	l := c.mustAcquire()
	e.clk.Advance(protocol.LeaseTTL + time.Second)
	if _, code, eb := c.heartbeat(l.Generation); code != 409 || eb.Code != api.CodeLeaseExpired {
		t.Fatalf("lease scaduta: atteso 409 lease_expired, ottenuto %d %v", code, eb)
	}
	resp := c.do("POST", api.PathHeartbeat, strings.NewReader("non json"))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("corpo non valido: atteso 400, ottenuto %d", resp.StatusCode)
	}
}

func TestOnlyOneOfManyDevicesGetsTheLease(t *testing.T) {
	e := newEnv(t)
	var clients []*client
	for i := 0; i < 20; i++ {
		clients = append(clients, e.enroll(fmt.Sprintf("Pl%02d", i)))
	}
	var wg sync.WaitGroup
	var wins, held atomic.Int32
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, code, eb := c.acquire()
			switch {
			case code == 200:
				wins.Add(1)
			case code == 409 && eb.Code == api.CodeLeaseHeld:
				held.Add(1)
			default:
				t.Errorf("risposta inattesa: %d %v", code, eb)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || held.Load() != 19 {
		t.Fatalf("vincitori = %d, rifiutati = %d, attesi 1 e 19", wins.Load(), held.Load())
	}
}

// ---------------------------------------------------------------------------
// Upload e download

func TestUploadThenDownloadRoundTrip(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	l := fede.mustAcquire()

	up := fede.mustUpload(l.Generation, 0, testWorld)
	if up.Version.Number != 1 || up.Version.Parent != 0 || up.Version.Author != "Fede" ||
		up.Version.Device != fede.device || up.Version.Generation != l.Generation {
		t.Fatalf("versione inattesa: %+v", up.Version)
	}
	if up.Snapshot == "" || up.AddedBytes <= 0 || up.Files != len(stored) {
		t.Fatalf("risposta inattesa: %+v", up)
	}

	st, _, _ := ale.status()
	if st.Pointer != 1 || st.Current == nil || st.Current.Number != 1 || st.Current.Author != "Fede" {
		t.Fatalf("lo stato deve mostrare la versione 1 di Fede: %+v", st)
	}

	// scarica Ale, che non è host: è permesso. Dentro non ci sono i file esclusi.
	code, files, hdr, eb := ale.download("")
	if code != 200 || !reflect.DeepEqual(files, stored) {
		t.Fatalf("download: %d %v\n got %v\nwant %v", code, eb, files, stored)
	}
	if hdr.Get(api.HeaderVersion) != "1" || hdr.Get("Content-Type") != "application/x-tar" {
		t.Fatalf("intestazioni inattese: %v", hdr)
	}
	if code, files, _, _ := ale.download("?version=1"); code != 200 || !reflect.DeepEqual(files, stored) {
		t.Fatalf("download della versione 1: %d %v", code, files)
	}

	if tmp := e.tmpEntries(); len(tmp) != 0 {
		t.Fatalf("restano file temporanei: %v", tmp)
	}
}

func TestSecondVersionAndOldVersionsStayDownloadable(t *testing.T) {
	e := newEnv(t)
	c := e.enroll("Fede")
	l := c.mustAcquire()
	c.mustUpload(l.Generation, 0, map[string]string{"level.dat": "uno"})
	up2 := c.mustUpload(l.Generation, 1, map[string]string{"level.dat": "due", "nuovo.txt": "x"})
	if up2.Version.Number != 2 || up2.Version.Parent != 1 {
		t.Fatalf("seconda versione inattesa: %+v", up2.Version)
	}
	if _, files, hdr, _ := c.download(""); hdr.Get(api.HeaderVersion) != "2" || files["level.dat"] != "due" {
		t.Fatalf("la corrente doveva essere la 2: %v %v", hdr, files)
	}
	if _, files, _, _ := c.download("?version=1"); !reflect.DeepEqual(files, map[string]string{"level.dat": "uno"}) {
		t.Fatalf("la versione 1 doveva essere intatta: %v", files)
	}
}

func TestOldHostCannotCommitButItsWorkIsKept(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")

	l1 := fede.mustAcquire()
	e.clk.Advance(protocol.LeaseTTL + time.Second) // Fede sparisce
	l2 := ale.mustAcquire()
	ale.mustUpload(l2.Generation, 0, map[string]string{"level.dat": "di Ale"})

	// Fede si risveglia e finisce il suo upload con la generazione vecchia
	code, _, eb := fede.uploadWorld(l1.Generation, 0, map[string]string{"level.dat": "di Fede"})
	if code != 409 || eb.Code != api.CodeStaleGeneration {
		t.Fatalf("atteso 409 stale_generation, ottenuto %d %v", code, eb)
	}
	if eb.Snapshot == "" {
		t.Fatal("la risposta deve dire dove è finito il lavoro rifiutato")
	}
	orphans := e.coord.Orphans()
	if len(orphans) != 1 || orphans[0].Request.Snapshot != eb.Snapshot || orphans[0].Request.Player != "Fede" {
		t.Fatalf("doveva esserci un orfano di Fede con quella copia: %+v", orphans)
	}
	if err := e.mem.Verify(context.Background(), archive.SnapshotID(eb.Snapshot)); err != nil {
		t.Fatalf("la copia dell'orfano doveva restare nell'archivio: %v", err)
	}
	if e.coord.Pointer() != 1 {
		t.Fatalf("il puntatore doveva restare sulla versione di Ale, è %d", e.coord.Pointer())
	}
	if _, files, _, _ := ale.download(""); files["level.dat"] != "di Ale" {
		t.Fatalf("la versione ufficiale doveva essere quella di Ale: %v", files)
	}
}

func TestUploadByNonHostIsRefusedAndKept(t *testing.T) {
	e := newEnv(t)
	gio := e.enroll("Gio")
	code, _, eb := gio.uploadWorld(1, 0, map[string]string{"level.dat": "x"})
	if code != 409 || eb.Code != api.CodeNoLease || eb.Snapshot == "" {
		t.Fatalf("atteso 409 no_lease con una copia conservata, ottenuto %d %v", code, eb)
	}
	if e.coord.Pointer() != 0 || len(e.coord.Versions()) != 0 {
		t.Fatal("nessuna versione doveva nascere")
	}
}

func TestUploadParentMismatchIsRefused(t *testing.T) {
	e := newEnv(t)
	c := e.enroll("Fede")
	l := c.mustAcquire()
	c.mustUpload(l.Generation, 0, map[string]string{"level.dat": "uno"})
	code, _, eb := c.uploadWorld(l.Generation, 0, map[string]string{"level.dat": "altro"})
	if code != 409 || eb.Code != api.CodeParentMismatch || eb.Snapshot == "" {
		t.Fatalf("atteso 409 parent_mismatch, ottenuto %d %v", code, eb)
	}
	if e.coord.Pointer() != 1 {
		t.Fatalf("il puntatore doveva restare 1, è %d", e.coord.Pointer())
	}
}

func TestUploadNeedsNumericParameters(t *testing.T) {
	e := newEnv(t)
	c := e.enroll("Fede")
	l := c.mustAcquire()
	body := packTree(t, map[string]string{"level.dat": "x"})
	for name, path := range map[string]string{
		"senza parametri":       api.PathUpload,
		"manca parent":          fmt.Sprintf("%s?generation=%d", api.PathUpload, l.Generation),
		"manca generation":      api.PathUpload + "?parent=0",
		"generation non numero": api.PathUpload + "?generation=x&parent=0",
		"parent negativo":       fmt.Sprintf("%s?generation=%d&parent=-1", api.PathUpload, l.Generation),
	} {
		resp := c.do("POST", path, bytes.NewReader(body))
		var eb api.ErrorBody
		_ = json.NewDecoder(resp.Body).Decode(&eb)
		resp.Body.Close()
		if resp.StatusCode != 400 || eb.Code != api.CodeBadRequest {
			t.Fatalf("%s: atteso 400 bad_request, ottenuto %d %v", name, resp.StatusCode, eb)
		}
	}
	if len(e.snapshots()) != 0 {
		t.Fatal("senza parametri validi non si deve salvare niente")
	}
}

func TestUploadRejectsBadStreams(t *testing.T) {
	good := packTree(t, map[string]string{
		"level.dat":        "livello",
		"region/r.0.0.mca": strings.Repeat("R", 3000),
	})
	damaged := append([]byte(nil), good...)
	damaged[bytes.Index(damaged, []byte("RRRR"))+10] = 'X'

	cases := map[string][]byte{
		"vuoto":                      {},
		"non è un tar":               []byte("ciao, io sono un upload"),
		"tagliato a metà":            good[:len(good)/2],
		"tagliato tra due voci":      good[:1536],
		"senza sigillo":              good[:len(good)-44],
		"byte cambiato":              damaged,
		"dati dopo il sigillo":       append(append([]byte(nil), good...), "x"...),
		"percorso che esce":          sealed(rawTar(t, &tar.Header{Typeflag: tar.TypeReg, Name: "../evil.txt", Mode: 0o644, Size: 4})),
		"percorso assoluto":          sealed(rawTar(t, &tar.Header{Typeflag: tar.TypeReg, Name: "/tmp/evil.txt", Mode: 0o644, Size: 4})),
		"collegamento simbolico":     sealed(rawTar(t, &tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc/passwd"})),
		"nessun file, solo cartelle": sealed(rawTar(t, &tar.Header{Typeflag: tar.TypeDir, Name: "a/", Mode: 0o755})),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			c := e.enroll("Fede")
			l := c.mustAcquire()
			code, _, eb := c.upload(l.Generation, 0, bytes.NewReader(body))
			if code != 400 || eb.Code != api.CodeBadArchive {
				t.Fatalf("atteso 400 bad_archive, ottenuto %d %v", code, eb)
			}
			if e.coord.Pointer() != 0 || len(e.snapshots()) != 0 || len(e.coord.Orphans()) != 0 {
				t.Fatal("un upload non valido non deve lasciare niente: né versione, né copia, né orfano")
			}
			if tmp := e.tmpEntries(); len(tmp) != 0 {
				t.Fatalf("restano file temporanei: %v", tmp)
			}
			// niente è uscito dalla cartella temporanea
			_ = filepath.WalkDir(e.root, func(p string, d fs.DirEntry, err error) error {
				if err == nil && strings.Contains(d.Name(), "evil") {
					t.Errorf("è comparso %s", p)
				}
				return nil
			})
			// e il servizio funziona ancora: un upload giusto subito dopo passa
			if code, _, eb := c.upload(l.Generation, 0, bytes.NewReader(good)); code != 200 {
				t.Fatalf("upload valido dopo quello rifiutato: %d %v", code, eb)
			}
		})
	}
}

func TestUploadLimits(t *testing.T) {
	e := newEnv(t, func(e *env) {
		e.cfg = func(c *Config) { c.MaxUploadBytes = 1000; c.MaxUploadFiles = 3 }
	})
	c := e.enroll("Fede")
	l := c.mustAcquire()

	if code, _, eb := c.uploadWorld(l.Generation, 0, map[string]string{"grande": strings.Repeat("x", 2000)}); code != 413 || eb.Code != api.CodeTooLarge {
		t.Fatalf("troppi byte: atteso 413 too_large, ottenuto %d %v", code, eb)
	}
	if code, _, eb := c.uploadWorld(l.Generation, 0, map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"}); code != 413 || eb.Code != api.CodeTooLarge {
		t.Fatalf("troppi file: atteso 413 too_large, ottenuto %d %v", code, eb)
	}
	// con la lunghezza dichiarata già oltre il massimo non si legge nemmeno
	resp := c.do("POST", c.uploadURL(l.Generation, 0), bytes.NewReader(make([]byte, 3<<20)))
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("corpo enorme: atteso 413, ottenuto %d", resp.StatusCode)
	}
	if len(e.snapshots()) != 0 || len(e.tmpEntries()) != 0 {
		t.Fatal("oltre i limiti non si salva niente")
	}
	if code, _, eb := c.uploadWorld(l.Generation, 0, map[string]string{"a": "1", "b": "2"}); code != 200 {
		t.Fatalf("entro i limiti doveva passare: %d %v", code, eb)
	}
}

func TestInterruptedUploadCommitsNothing(t *testing.T) {
	e := newEnv(t)
	c := e.enroll("Fede")
	l := c.mustAcquire()
	full := packTree(t, map[string]string{
		"level.dat":        "livello",
		"region/r.0.0.mca": strings.Repeat("R", 20000),
	})

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		resp, err := c.tryDo("POST", c.uploadURL(l.Generation, 0), pr)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	if _, err := pw.Write(full[:len(full)/2]); err != nil {
		t.Fatal(err)
	}
	eventually(t, "il server deve aver preso il trasferimento", func() bool { return e.srv.InFlight() == 1 })
	pw.CloseWithError(errors.New("rete caduta")) // il client sparisce a metà
	if err := <-done; err == nil {
		t.Fatal("il client doveva vedere l'errore")
	}

	eventually(t, "il server deve chiudere il trasferimento", func() bool { return e.srv.InFlight() == 0 })
	if e.coord.Pointer() != 0 || len(e.snapshots()) != 0 || len(e.coord.Orphans()) != 0 {
		t.Fatal("un upload interrotto non deve lasciare né versione né copia")
	}
	if tmp := e.tmpEntries(); len(tmp) != 0 {
		t.Fatalf("restano file temporanei: %v", tmp)
	}
	if code, _, eb := c.upload(l.Generation, 0, bytes.NewReader(full)); code != 200 {
		t.Fatalf("il client riprova da capo e passa: %d %v", code, eb)
	}
}

func TestStalledUploadIsDropped(t *testing.T) {
	e := newEnv(t, func(e *env) {
		e.cfg = func(c *Config) { c.TransferIdle = 300 * time.Millisecond }
	})
	c := e.enroll("Fede")
	l := c.mustAcquire()
	full := packTree(t, map[string]string{"region/r.0.0.mca": strings.Repeat("R", 20000)})

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := c.tryDo("POST", c.uploadURL(l.Generation, 0), pr); err == nil {
			resp.Body.Close()
		}
	}()
	if _, err := pw.Write(full[:2000]); err != nil {
		t.Fatal(err)
	}
	eventually(t, "il server deve aver preso il trasferimento", func() bool { return e.srv.InFlight() == 1 })
	// il client resta fermo, senza chiudere: il server deve rinunciare da solo
	eventually(t, "il server deve lasciare andare il client fermo", func() bool { return e.srv.InFlight() == 0 })
	pw.Close()
	<-done
	if e.coord.Pointer() != 0 || len(e.snapshots()) != 0 {
		t.Fatal("un upload fermo non deve lasciare niente")
	}
}

func TestTransfersAreLimited(t *testing.T) {
	e := newEnv(t, func(e *env) {
		e.cfg = func(c *Config) { c.MaxTransfers = 1 }
	})
	c := e.enroll("Fede")
	l := c.mustAcquire()
	c.mustUpload(l.Generation, 0, map[string]string{"level.dat": "uno"})
	full := packTree(t, map[string]string{"region/r.0.0.mca": strings.Repeat("R", 20000)})

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := c.tryDo("POST", c.uploadURL(l.Generation, 1), pr); err == nil {
			resp.Body.Close()
		}
	}()
	if _, err := pw.Write(full[:1000]); err != nil {
		t.Fatal(err)
	}
	eventually(t, "il primo trasferimento deve occupare il posto", func() bool { return e.srv.InFlight() == 1 })

	resp := c.do("GET", api.PathDownload, nil)
	var eb api.ErrorBody
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	resp.Body.Close()
	if resp.StatusCode != 503 || eb.Code != api.CodeBusy || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("download a posti finiti: atteso 503 busy con Retry-After, ottenuto %d %v", resp.StatusCode, eb)
	}
	if code, _, eb := c.upload(l.Generation, 1, bytes.NewReader(full)); code != 503 || eb.Code != api.CodeBusy {
		t.Fatalf("upload a posti finiti: atteso 503 busy, ottenuto %d %v", code, eb)
	}
	// le richieste leggere non sono limitate
	if _, code, _ := c.status(); code != 200 {
		t.Fatal("lo stato deve rispondere anche a posti finiti")
	}
	if _, code, _ := c.heartbeat(l.Generation); code != 200 {
		t.Fatal("l'heartbeat deve passare anche a posti finiti")
	}

	pw.CloseWithError(errors.New("basta"))
	<-done
	eventually(t, "il posto deve liberarsi", func() bool { return e.srv.InFlight() == 0 })
	if code, files, _, _ := c.download(""); code != 200 || files["level.dat"] != "uno" {
		t.Fatalf("a posto libero il download doveva passare: %d %v", code, files)
	}
}

func TestDownloadErrors(t *testing.T) {
	e := newEnv(t)
	c := e.enroll("Fede")
	if code, _, _, eb := c.download(""); code != 404 || eb.Code != api.CodeNoVersion {
		t.Fatalf("senza versioni: atteso 404 no_version, ottenuto %d %v", code, eb)
	}
	l := c.mustAcquire()
	c.mustUpload(l.Generation, 0, map[string]string{"level.dat": "x"})
	for query, want := range map[string]string{
		"?version=99":  api.CodeUnknownVersion,
		"?version=0":   api.CodeUnknownVersion,
		"?version=abc": api.CodeBadRequest,
		"?version=":    api.CodeBadRequest,
		"?version=-1":  api.CodeBadRequest,
	} {
		code, _, _, eb := c.download(query)
		if eb.Code != want || (want == api.CodeUnknownVersion && code != 404) || (want == api.CodeBadRequest && code != 400) {
			t.Fatalf("download%s: atteso %s, ottenuto %d %v", query, want, code, eb)
		}
	}
}

func TestCorruptCopyIsNeverServed(t *testing.T) {
	e := newEnv(t)
	c := e.enroll("Fede")
	l := c.mustAcquire()
	up := c.mustUpload(l.Generation, 0, map[string]string{"level.dat": "livello", "region/r.0.0.mca": "regione"})
	if err := e.mem.Corrupt(archive.SnapshotID(up.Snapshot), "level.dat"); err != nil {
		t.Fatal(err)
	}
	resp := c.do("GET", api.PathDownload, nil)
	defer resp.Body.Close()
	var eb api.ErrorBody
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	if resp.StatusCode != 500 || eb.Code != api.CodeArchive || strings.Contains(resp.Header.Get("Content-Type"), "tar") {
		t.Fatalf("copia rovinata: atteso un errore 500 archive_error (non un flusso), ottenuto %d %v", resp.StatusCode, eb)
	}
	if tmp := e.tmpEntries(); len(tmp) != 0 {
		t.Fatalf("restano file temporanei: %v", tmp)
	}
}

// failingStore fa fallire il salvataggio con un errore che contiene dettagli
// che un client non deve mai leggere.
type failingStore struct {
	archive.Store
	err error
}

func (f failingStore) Put(context.Context, string) (archive.PutResult, error) {
	return archive.PutResult{}, f.err
}

func TestInternalErrorsDoNotLeakDetails(t *testing.T) {
	secretErr := errors.New(`restic: open /srv/mcsync/repo/config: permesso negato (password=hunter2)`)
	e := newEnv(t, func(e *env) { e.store = failingStore{Store: e.mem, err: secretErr} })
	c := e.enroll("Fede")
	l := c.mustAcquire()

	resp := c.do("POST", c.uploadURL(l.Generation, 0), bytes.NewReader(packTree(t, map[string]string{"level.dat": "x"})))
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var eb api.ErrorBody
	if err := json.Unmarshal(raw, &eb); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 500 || eb.Code != api.CodeInternal {
		t.Fatalf("atteso 500 internal, ottenuto %d %v", resp.StatusCode, eb)
	}
	for _, leak := range []string{"restic", "/srv", "hunter2", "permesso"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("la risposta rivela %q: %s", leak, raw)
		}
	}
	id := resp.Header.Get(api.HeaderRequestID)
	if id == "" || eb.RequestID != id {
		t.Fatalf("l'identificativo di richiesta deve essere in intestazione e nel corpo: %q %q", id, eb.RequestID)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "hunter2") || !strings.Contains(logs, id) {
		t.Fatalf("il dettaglio dell'errore deve stare nel log insieme all'identificativo:\n%s", logs)
	}
	if e.coord.Pointer() != 0 {
		t.Fatal("se il salvataggio fallisce il puntatore non si muove")
	}
}

// flakySaver fa fallire il salvataggio dello stato a richiesta.
type flakySaver struct {
	fail atomic.Bool
	mu   sync.Mutex
	last protocol.State
}

func (s *flakySaver) Save(st protocol.State) error {
	if s.fail.Load() {
		return errors.New("disco pieno in /srv/mcsync/state")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = st
	return nil
}

func TestPersistFailureIsReportedAndChangesNothing(t *testing.T) {
	saver := &flakySaver{}
	e := newEnv(t, func(e *env) { e.saver = saver })
	c := e.enroll("Fede")

	saver.fail.Store(true)
	resp := c.do("POST", api.PathAcquire, nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var eb api.ErrorBody
	_ = json.Unmarshal(raw, &eb)
	if resp.StatusCode != 500 || eb.Code != api.CodePersist || strings.Contains(string(raw), "/srv") {
		t.Fatalf("atteso 500 persist_failed senza dettagli, ottenuto %d %s", resp.StatusCode, raw)
	}
	if st, _, _ := c.status(); st.Lease != nil {
		t.Fatalf("senza salvataggio la lease non deve esistere: %+v", st.Lease)
	}

	saver.fail.Store(false)
	if l := c.mustAcquire(); l.Generation != 1 {
		t.Fatalf("la generazione non doveva essere consumata dal tentativo fallito: %+v", l)
	}
}

// ---------------------------------------------------------------------------
// Riavvio, log, indirizzo di ascolto

func TestStateAndDevicesSurviveRestart(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	l := fede.mustAcquire()
	fede.mustUpload(l.Generation, 0, testWorld)
	ale.acquire() // rifiutata: la lease è di Fede (non cambia niente)

	// resti di un trasferimento interrotto dal riavvio
	stale := filepath.Join(e.work, "tmp", "up-vecchio")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(stale, "pezzo"), []byte("x"), 0o644)

	e.restart()

	if tmp := e.tmpEntries(); len(tmp) != 0 {
		t.Fatalf("all'avvio i resti dei trasferimenti vanno puliti: %v", tmp)
	}
	st, code, eb := fede.status() // lo stesso token funziona dopo il riavvio
	if code != 200 {
		t.Fatalf("status dopo il riavvio: %d %v", code, eb)
	}
	if st.Pointer != 1 || st.Lease == nil || st.Lease.Player != "Fede" || st.Lease.Generation != l.Generation {
		t.Fatalf("pointer e lease dovevano sopravvivere: %+v", st)
	}
	if _, code, eb := ale.acquire(); code != 409 || eb.Code != api.CodeLeaseHeld {
		t.Fatalf("la lease di Fede vale ancora: atteso 409 lease_held, ottenuto %d %v", code, eb)
	}
	if _, code, _ := fede.heartbeat(l.Generation); code != 200 {
		t.Fatal("Fede doveva poter continuare con la sua lease dopo il riavvio del servizio")
	}
	if code, files, _, _ := ale.download(""); code != 200 || !reflect.DeepEqual(files, stored) {
		t.Fatalf("il mondo doveva essere ancora scaricabile: %d %v", code, files)
	}
	// il vecchio host non può tornare indietro nel tempo: la generazione continua
	e.clk.Advance(protocol.LeaseTTL + time.Second)
	if l2, code, _ := ale.acquire(); code != 200 || l2.Generation != l.Generation+1 {
		t.Fatalf("la generazione doveva continuare dopo il riavvio: %d %+v", code, l2)
	}
}

func TestRevocationSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	c := e.enroll("Fede")
	if err := e.auth.Revoke(c.device); err != nil {
		t.Fatal(err)
	}
	e.restart()
	if _, code, _ := c.status(); code != 401 {
		t.Fatalf("un dispositivo revocato doveva restarlo dopo il riavvio, ottenuto %d", code)
	}
}

func TestLogsNeverContainSecrets(t *testing.T) {
	e := newEnv(t)
	fede := e.enroll("Fede")
	ale := e.enroll("Ale")
	l := fede.mustAcquire()
	fede.mustUpload(l.Generation, 0, testWorld)
	fede.release(l.Generation, "Ale")
	ale.mustAcquire()
	ale.download("")
	e.anon().call("POST", api.PathEnroll, api.EnrollRequest{Invite: "inv_sbagliato_ma_segreto"}, nil)
	e.anon().do("GET", api.PathStatus, nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer mcs_tentativo_segreto") }).Body.Close()

	logs := e.logs.String()
	if len(e.secrets) == 0 || len(logs) == 0 {
		t.Fatal("il test non ha niente da controllare")
	}
	for _, s := range append(e.secrets, "inv_sbagliato_ma_segreto", "mcs_tentativo_segreto") {
		if strings.Contains(logs, s) {
			t.Fatalf("un segreto è finito nel log: %q", s)
		}
	}
	if !strings.Contains(logs, "upload confermato") || !strings.Contains(logs, "Fede") {
		t.Fatalf("il log doveva raccontare cosa è successo:\n%s", logs)
	}
}

func TestCheckListenAddr(t *testing.T) {
	good := []string{"127.0.0.1:8080", "100.64.12.7:8080", "[::1]:8080", "[fd7a:115c:a1e0::1]:8080", "127.0.0.1:0"}
	bad := []string{
		"0.0.0.0:8080", "[::]:8080", ":8080", "8080", "", "localhost:8080", "example.com:8080",
		"127.0.0.1", "127.0.0.1:abc", "127.0.0.1:70000", "127.0.0.1:-1",
	}
	for _, a := range good {
		if err := CheckListenAddr(a); err != nil {
			t.Errorf("%q doveva andare bene: %v", a, err)
		}
	}
	for _, a := range bad {
		if err := CheckListenAddr(a); err == nil {
			t.Errorf("%q doveva essere rifiutato", a)
		}
	}
}

func TestListenAndServeRefusesWildcard(t *testing.T) {
	e := newEnv(t)
	if err := e.srv.ListenAndServe(context.Background(), "0.0.0.0:0"); err == nil {
		t.Fatal("doveva rifiutare 0.0.0.0")
	}
}

func TestServeAndGracefulShutdown(t *testing.T) {
	e := newEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.srv.Serve(ctx, ln) }()

	c := e.enroll("Fede") // l'iscrizione passa dal server di prova; qui si prova l'altro
	c.base = "http://" + ln.Addr().String()
	if _, code, eb := c.status(); code != 200 {
		t.Fatalf("status dal server vero: %d %v", code, eb)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("lo spegnimento pulito doveva restituire nil, ottenuto %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("il server non si è fermato")
	}
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), 500*time.Millisecond); err == nil {
		t.Fatal("dopo lo spegnimento la porta doveva essere chiusa")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	e := newEnv(t)
	if _, err := New(Config{}); err == nil {
		t.Fatal("una configurazione vuota doveva essere rifiutata")
	}
	if _, err := New(Config{Coordinator: e.coord, Service: service.New(e.coord, e.mem), Auth: e.auth}); err == nil {
		t.Fatal("senza cartella di lavoro doveva essere rifiutata")
	}
}
