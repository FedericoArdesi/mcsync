package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FedericoArdesi/mcsync/internal/admin"
	"github.com/FedericoArdesi/mcsync/internal/api"
	"github.com/FedericoArdesi/mcsync/internal/archive"
	"github.com/FedericoArdesi/mcsync/internal/auth"
	"github.com/FedericoArdesi/mcsync/internal/protocol"
	"github.com/FedericoArdesi/mcsync/internal/tarutil"
	"github.com/FedericoArdesi/mcsync/internal/testutil"
)

func runCmd(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

// shortDir dà una cartella con percorso corto: i socket Unix hanno un limite di lunghezza.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "mcs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func codeFrom(t *testing.T, stdout string) (addr, secret string) {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "mcs1.") {
			a, s, err := api.DecodeInviteCode(line)
			if err != nil {
				t.Fatal(err)
			}
			return a, s
		}
	}
	t.Fatalf("nessun codice di invito nell'output:\n%s", stdout)
	return
}

func TestUsageErrors(t *testing.T) {
	data := t.TempDir()
	for name, args := range map[string][]string{
		"nessun comando":             {},
		"comando sconosciuto":        {"boh"},
		"invite senza addr":          {"invite", "--data", data, "Fede"},
		"invite senza giocatore":     {"invite", "--data", data, "--addr", "100.64.0.1:8765"},
		"invite con nome non valido": {"invite", "--data", data, "--addr", "100.64.0.1:8765", "../x"},
		"invite con addr jolly":      {"invite", "--data", data, "--addr", "0.0.0.0:8765", "Fede"},
		"revoke senza id":            {"revoke", "--data", data},
		"serve senza listen":         {"serve", "--data", data},
		"serve con indirizzo jolly":  {"serve", "--data", data, "--listen", "0.0.0.0:8765"},
		"opzione sconosciuta":        {"devices", "--data", data, "--boh"},
	} {
		if code, _, _ := runCmd(t, args...); code != 2 {
			t.Errorf("%s: atteso codice 2, ottenuto %d", name, code)
		}
	}
	if code, out, _ := runCmd(t, "help"); code != 0 || !strings.Contains(out, "mcsyncd") {
		t.Errorf("help: %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(data, "state", "devices.json")); err == nil {
		t.Error("gli errori d'uso non devono creare file")
	}
}

func TestInviteDevicesRevokeWhenServiceIsStopped(t *testing.T) {
	data := t.TempDir()
	code, out, errs := runCmd(t, "invite", "--data", data, "--addr", "100.64.0.1:8765", "Fede")
	if code != 0 {
		t.Fatalf("invite: %d %s", code, errs)
	}
	addr, secret := codeFrom(t, out)
	if addr != "100.64.0.1:8765" {
		t.Fatalf("indirizzo nel codice = %q", addr)
	}

	// il file creato è quello che il servizio leggerà all'avvio
	store, err := auth.Open(filepath.Join(data, "state", "devices.json"), protocol.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	dev, token, err := store.Redeem(secret, "PC di Fede")
	if err != nil {
		t.Fatal(err)
	}

	code, out, _ = runCmd(t, "devices", "--data", data)
	if code != 0 || !strings.Contains(out, dev.ID) || !strings.Contains(out, "Fede") || !strings.Contains(out, "attivo") {
		t.Fatalf("devices: %d\n%s", code, out)
	}
	if strings.Contains(out, token) {
		t.Fatal("l'elenco non deve mostrare i token")
	}
	if code, _, errs := runCmd(t, "revoke", "--data", data, dev.ID); code != 0 {
		t.Fatalf("revoke: %d %s", code, errs)
	}
	if _, out, _ = runCmd(t, "devices", "--data", data); !strings.Contains(out, "revocato") {
		t.Fatalf("il dispositivo doveva risultare revocato:\n%s", out)
	}
	if code, _, errs := runCmd(t, "revoke", "--data", data, "dev-inesistente"); code != 1 || !strings.Contains(errs, "dispositivo sconosciuto") {
		t.Fatalf("revoke di un ID sconosciuto: %d %s", code, errs)
	}
}

func TestEmptyDevicesList(t *testing.T) {
	if code, out, _ := runCmd(t, "devices", "--data", t.TempDir()); code != 0 || !strings.Contains(out, "Nessun dispositivo") {
		t.Fatalf("%d %q", code, out)
	}
}

// Con il servizio acceso i comandi passano dal socket: l'invito è subito
// valido per il servizio, che tiene i dispositivi in memoria.
func TestCommandsGoThroughRunningService(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("socket Unix: il servizio gira su Linux")
	}
	data := shortDir(t)
	l := layout{data}
	if err := os.MkdirAll(l.state(), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := auth.Open(l.devices(), protocol.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- admin.Serve(ctx, l.sock(), admin.Handler(store)) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(5 * time.Second); !admin.Running(l.sock()); {
		if time.Now().After(deadline) {
			t.Fatal("il socket non è partito")
		}
		time.Sleep(10 * time.Millisecond)
	}

	code, out, errs := runCmd(t, "invite", "--data", data, "--addr", "100.64.0.1:8765", "Ale")
	if code != 0 {
		t.Fatalf("invite: %d %s", code, errs)
	}
	_, secret := codeFrom(t, out)
	dev, token, err := store.Redeem(secret, "PC")
	if err != nil {
		t.Fatalf("l'invito doveva valere per il servizio in funzione: %v", err)
	}
	if code, _, errs := runCmd(t, "revoke", "--data", data, dev.ID); code != 0 {
		t.Fatalf("revoke: %d %s", code, errs)
	}
	if _, err := store.Authenticate(token); err == nil {
		t.Fatal("la revoca doveva valere subito per il servizio in funzione")
	}
	if code, _, errs := runCmd(t, "revoke", "--data", data, "dev-inesistente"); code != 1 || !strings.Contains(errs, "dispositivo sconosciuto") {
		t.Fatalf("errore dal servizio: %d %s", code, errs)
	}
}

// ---------------------------------------------------------------------------
// Prova completa: invito da riga di comando, iscrizione, lease, upload, download.

type httpc struct {
	t     *testing.T
	base  string
	token string
}

func (c httpc) do(method, path string, body io.Reader) *http.Response {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, body)
	req.Header.Set(api.HeaderProtocol, strconv.Itoa(api.ProtocolVersion))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp
}

func (c httpc) json(method, path string, in, out any) int {
	c.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	resp := c.do(method, path, body)
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			c.t.Fatal(err)
		}
	}
	return resp.StatusCode
}

// roundTrip fa tutto il giro contro il server raggiungibile a base: iscrizione
// con l'invito, lease, upload, download e confronto del contenuto.
func roundTrip(t *testing.T, base, secret string) {
	t.Helper()
	anon := httpc{t: t, base: base}
	var enr api.EnrollResponse
	if code := anon.json("POST", api.PathEnroll, api.EnrollRequest{Invite: secret, Label: "PC di prova"}, &enr); code != 200 {
		t.Fatalf("iscrizione: %d", code)
	}
	c := httpc{t: t, base: base, token: enr.Token}
	var lease api.Lease
	if code := c.json("POST", api.PathAcquire, nil, &lease); code != 200 {
		t.Fatalf("acquire: %d", code)
	}

	world := map[string]string{"level.dat": "livello", "region/r.0.0.mca": strings.Repeat("R", 4000), "players/a.dat": "giocatore"}
	src := filepath.Join(t.TempDir(), "w")
	testutil.WriteTree(t, src, world)
	testutil.WriteTree(t, src, map[string]string{"session.lock": "x"})
	var tarBuf bytes.Buffer
	if err := tarutil.Pack(&tarBuf, src, nil); err != nil {
		t.Fatal(err)
	}
	var up api.UploadResponse
	path := fmt.Sprintf("%s?%s=%d&%s=0", api.PathUpload, api.QueryGeneration, lease.Generation, api.QueryParent)
	if code := c.json("POST", path, nil, nil); code != 400 { // senza corpo: archivio non valido
		t.Fatalf("upload vuoto: atteso 400, ottenuto %d", code)
	}
	resp := c.do("POST", path, &tarBuf)
	if err := json.NewDecoder(resp.Body).Decode(&up); err != nil || resp.StatusCode != 200 {
		t.Fatalf("upload: %d %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	if up.Version.Number != 1 || up.Files != 3 {
		t.Fatalf("upload inatteso: %+v", up)
	}

	resp = c.do("GET", api.PathDownload, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download: %d", resp.StatusCode)
	}
	dst := filepath.Join(t.TempDir(), "dl")
	if err := tarutil.Unpack(resp.Body, dst, tarutil.Limits{}); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ReadTree(t, dst); !reflect.DeepEqual(got, world) {
		t.Fatalf("contenuto diverso:\n got %v\nwant %v", got, world)
	}
}

func TestBuildWiresEverythingTogether(t *testing.T) {
	data := t.TempDir()
	l := layout{data}
	srv, devs, err := build(l, archive.NewMemStore(), protocol.SystemClock{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	secret, err := devs.CreateInvite("Fede")
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ts.URL, secret)

	// stato e dispositivi sono finiti su disco
	for _, f := range []string{l.stateFile(), l.devices()} {
		if st, err := os.Stat(f); err != nil || st.Size() == 0 {
			t.Fatalf("manca %s: %v", f, err)
		}
	}
}

func TestFreeBytesFuncKeepsReserve(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("controllo dello spazio non disponibile su Windows")
	}
	free, err := freeSpace(t.TempDir())
	if err != nil || free == 0 {
		t.Skipf("spazio libero non leggibile: %d %v", free, err)
	}
	if got := freeBytesFunc(t.TempDir(), free+(1<<40))(); got != 0 {
		t.Fatalf("con una riserva più grande dello spazio libero non deve restare niente, resta %d", got)
	}
	if got := freeBytesFunc(t.TempDir(), 0)(); got <= 0 {
		t.Fatalf("senza riserva deve restare spazio, resta %d", got)
	}
}

// ---------------------------------------------------------------------------
// Con restic vero (si salta se restic non c'è: variabile RESTIC_BIN o nel PATH).

func resticBin(t *testing.T) string {
	t.Helper()
	if b := os.Getenv("RESTIC_BIN"); b != "" {
		return b
	}
	b, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic non trovato: imposta RESTIC_BIN o mettilo nel PATH")
	}
	return b
}

func TestInitIsSafeToRepeat(t *testing.T) {
	bin := resticBin(t)
	data := shortDir(t)
	code, out, errs := runCmd(t, "init", "--data", data, "--restic", bin)
	if code != 0 || !strings.Contains(out, "Creata la chiave") {
		t.Fatalf("init: %d\n%s\n%s", code, out, errs)
	}
	l := layout{data}
	key1, _ := os.ReadFile(l.key())
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(l.key()); st.Mode().Perm() != 0o600 {
			t.Fatalf("la chiave deve essere leggibile solo dal proprietario, è %v", st.Mode().Perm())
		}
	}
	code, out, errs = runCmd(t, "init", "--data", data, "--restic", bin)
	if code != 0 || strings.Contains(out, "Creata la chiave") {
		t.Fatalf("secondo init: %d\n%s\n%s", code, out, errs)
	}
	if key2, _ := os.ReadFile(l.key()); !bytes.Equal(key1, key2) {
		t.Fatal("un secondo init non deve cambiare la chiave: i mondi salvati non si leggerebbero più")
	}
}

func TestServeWithRealRestic(t *testing.T) {
	bin := resticBin(t)
	data := shortDir(t)
	if code, _, errs := runCmd(t, "init", "--data", data, "--restic", bin); code != 0 {
		t.Fatalf("init: %d %s", code, errs)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	// l'invito si crea a servizio fermo (vale anche su Windows)
	code, out, errs := runCmd(t, "invite", "--data", data, "--addr", addr, "Fede")
	if code != 0 {
		t.Fatalf("invite: %d %s", code, errs)
	}
	_, secret := codeFrom(t, out)

	ctx, cancel := context.WithCancel(context.Background())
	var serveOut, serveErr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--data", data, "--listen", addr, "--restic", bin, "--reserve-gb", "0"}, &serveOut, &serveErr)
	}()
	defer cancel()
	for deadline := time.Now().Add(15 * time.Second); ; {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			break
		}
		select {
		case code := <-done:
			t.Fatalf("il servizio si è fermato subito (%d):\n%s", code, serveErr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("il servizio non è partito:\n%s", serveErr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	roundTrip(t, "http://"+addr, secret)

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("lo spegnimento doveva dare codice 0, dato %d:\n%s", code, serveErr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("il servizio non si è fermato")
	}
	if strings.Contains(serveErr.String(), secret) {
		t.Fatal("il codice di invito è finito nel log")
	}
}

func TestServeNeedsInit(t *testing.T) {
	code, _, errs := runCmd(t, "serve", "--data", t.TempDir(), "--listen", "127.0.0.1:0")
	if code != 1 || !strings.Contains(errs, "mcsyncd init") {
		t.Fatalf("senza init: atteso un errore che lo spieghi, ottenuto %d %q", code, errs)
	}
}
