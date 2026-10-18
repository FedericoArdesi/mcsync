package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mcsync/internal/api"
	"mcsync/internal/tarutil"
)

// memService è un servizio completo ma in memoria, abbastanza fedele da far
// girare l'agente vero attraverso HTTP: un solo host, versioni come flussi.
type memService struct {
	mu       sync.Mutex
	inviteOK bool
	gen      uint64
	holder   string
	pointer  uint64
	versions map[uint64][]byte
	log      []string
}

func (s *memService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func(code int, c, m string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(api.ErrorBody{Code: c, Message: m})
	}
	ok := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	s.log = append(s.log, r.Method+" "+r.URL.Path)
	if r.URL.Path == api.PathEnroll {
		var req api.EnrollRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Invite != "segreto" || !s.inviteOK {
			fail(403, api.CodeBadInvite, "invito non valido")
			return
		}
		s.inviteOK = false
		ok(api.EnrollResponse{DeviceID: "dev-" + req.Label, Player: "Fede", Token: "tok-" + req.Label})
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
	if tok == r.Header.Get("Authorization") || tok == "" {
		fail(401, api.CodeUnauthorized, "token")
		return
	}
	me := api.Identity{Player: "Fede", Device: "dev-" + tok}
	lease := func() *api.Lease {
		if s.holder == "" {
			return nil
		}
		return &api.Lease{Player: "Fede", Device: s.holder, Generation: s.gen, ExpiresInSeconds: 300}
	}
	switch r.URL.Path {
	case api.PathStatus:
		st := api.Status{Protocol: 1, You: me, Pointer: s.pointer, Lease: lease()}
		if s.pointer != 0 {
			st.Current = &api.Version{Number: s.pointer, Author: "Fede", Device: me.Device, Generation: s.gen}
		}
		ok(st)
	case api.PathAcquire:
		if s.holder != "" && s.holder != me.Device {
			fail(409, api.CodeLeaseHeld, "gioca già un altro")
			return
		}
		if s.holder == "" {
			s.gen++
		}
		s.holder = me.Device
		ok(*lease())
	case api.PathHeartbeat, api.PathRelease:
		var body struct{ Generation uint64 }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if s.holder != me.Device || body.Generation != s.gen {
			fail(409, api.CodeStaleGeneration, "non sei host")
			return
		}
		if r.URL.Path == api.PathRelease {
			s.holder = ""
			w.WriteHeader(204)
			return
		}
		ok(*lease())
	case api.PathUpload:
		gen, _ := strconv.ParseUint(r.URL.Query().Get(api.QueryGeneration), 10, 64)
		parent, _ := strconv.ParseUint(r.URL.Query().Get(api.QueryParent), 10, 64)
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		// il flusso deve essere integro: lo si scompatta davvero
		chk := filepath.Join(os.TempDir(), "memsvc-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		defer os.RemoveAll(chk)
		if err := tarutil.Unpack(bytes.NewReader(buf.Bytes()), chk, tarutil.Limits{MaxFiles: 1000, MaxBytes: 1 << 30}); err != nil {
			fail(400, api.CodeBadArchive, err.Error())
			return
		}
		if s.holder != me.Device || gen != s.gen {
			fail(409, api.CodeStaleGeneration, "non sei host")
			return
		}
		if parent != s.pointer {
			fail(409, api.CodeParentMismatch, "base sbagliata")
			return
		}
		s.pointer++
		s.versions[s.pointer] = buf.Bytes()
		ok(api.UploadResponse{Version: api.Version{Number: s.pointer, Parent: parent, Device: me.Device, Generation: gen}, Files: 1})
	case api.PathDownload:
		if s.pointer == 0 {
			fail(404, api.CodeNoVersion, "nessuna versione")
			return
		}
		w.Header().Set(api.HeaderVersion, strconv.FormatUint(s.pointer, 10))
		_, _ = w.Write(s.versions[s.pointer])
	default:
		fail(404, api.CodeBadRequest, "?")
	}
}

func cli(t *testing.T, ctx context.Context, args ...string) (int, string, string) {
	t.Helper()
	var o, e bytes.Buffer
	code := run(ctx, args, &o, &e)
	return code, o.String(), e.String()
}

func TestFullFlowAcrossTwoPCs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Log("giro su Windows")
	}
	svc := &memService{inviteOK: true, versions: map[uint64][]byte{}}
	srv := httptest.NewServer(svc)
	defer srv.Close()
	code := api.EncodeInviteCode(srv.Listener.Addr().String(), "segreto")
	ctx := context.Background()

	pc1 := filepath.Join(t.TempDir(), "pc1.json")
	pc2 := filepath.Join(t.TempDir(), "pc2.json")

	// prima di iscriversi
	if c, _, e := cli(t, ctx, "status", "--config", pc1); c != 1 || !strings.Contains(e, "non è iscritto") {
		t.Fatalf("status senza iscrizione: code=%d err=%q", c, e)
	}
	// iscrizione
	if c, o, e := cli(t, ctx, "enroll", code, "--label", "uno", "--config", pc1); c != 0 {
		t.Fatalf("enroll: code=%d out=%q err=%q", c, o, e)
	}
	if st, err := os.Stat(pc1); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("permessi del file di configurazione = %v", st.Mode().Perm())
	}
	// stesso invito di nuovo: rifiutato con un messaggio chiaro, e nessun file
	if c, _, e := cli(t, ctx, "enroll", code, "--label", "due", "--config", pc2); c != 1 || !strings.Contains(e, "già usato") {
		t.Fatalf("invito riusato: code=%d err=%q", c, e)
	}
	if _, err := os.Stat(pc2); err == nil {
		t.Error("creato un file di configurazione dopo un'iscrizione fallita")
	}
	// iscrizione ripetuta sullo stesso PC: non sovrascrive
	if c, _, e := cli(t, ctx, "enroll", code, "--config", pc1); c != 1 || !strings.Contains(e, "già iscritto") {
		t.Fatalf("doppia iscrizione: code=%d err=%q", c, e)
	}
	svc.mu.Lock()
	svc.inviteOK = true
	svc.mu.Unlock()
	if c, _, e := cli(t, ctx, "enroll", code, "--label", "due", "--config", pc2); c != 0 {
		t.Fatalf("enroll pc2: code=%d err=%q", c, e)
	}

	// sessione sul PC 1, chiusa con "Ctrl+C"
	world1 := filepath.Join(t.TempDir(), "mondo")
	rctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	c, o, e := cli(t, rctx, "run", "--config", pc1, "--world", world1,
		"--fake-every", "20ms", "--upload-every", "100ms", "--heartbeat-every", "50ms")
	cancel()
	if c != 0 || !strings.Contains(o, "Fatto") {
		t.Fatalf("run pc1: code=%d out=%q err=%q", c, o, e)
	}
	if svc.pointer == 0 || svc.holder != "" {
		t.Fatalf("pointer=%d holder=%q: atteso mondo salvato e lease rilasciata", svc.pointer, svc.holder)
	}
	if c, o, _ := cli(t, ctx, "status", "--config", pc2); c != 0 || !strings.Contains(o, "Host: nessuno") || !strings.Contains(o, "Versione ufficiale: "+strconv.FormatUint(svc.pointer, 10)) {
		t.Errorf("status dopo la sessione: code=%d out=%q", c, o)
	}

	// sessione sul PC 2: deve ritrovare il mondo del PC 1
	ticks1, _ := os.ReadFile(filepath.Join(world1, "ticks.txt"))
	world2 := filepath.Join(t.TempDir(), "mondo")
	var got string
	stopWatch := make(chan struct{})
	go func() { // legge il mondo appena arrivato, prima che il gioco lo cambi troppo
		for {
			select {
			case <-stopWatch:
				return
			default:
			}
			if b, err := os.ReadFile(filepath.Join(world2, "level.dat")); err == nil {
				got = string(b)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	rctx2, cancel2 := context.WithTimeout(ctx, 250*time.Millisecond)
	c, o, e = cli(t, rctx2, "run", "--config", pc2, "--world", world2,
		"--fake-every", "20ms", "--upload-every", "100ms", "--heartbeat-every", "50ms")
	cancel2()
	close(stopWatch)
	if c != 0 {
		t.Fatalf("run pc2: code=%d out=%q err=%q", c, o, e)
	}
	if got != "mondo finto\n" {
		t.Errorf("il PC 2 non ha ricevuto il mondo del PC 1: level.dat = %q", got)
	}
	if len(ticks1) == 0 {
		t.Error("il PC 1 non ha prodotto niente")
	}
	if !strings.Contains(e, "mondo aggiornato") {
		t.Errorf("il PC 2 non ha scaricato: log = %q", e)
	}
}

func TestSecondPlayerIsToldWhoIsPlaying(t *testing.T) {
	svc := &memService{inviteOK: true, versions: map[uint64][]byte{}, holder: "dev-altro", gen: 5}
	srv := httptest.NewServer(svc)
	defer srv.Close()
	cfg := filepath.Join(t.TempDir(), "c.json")
	b, _ := json.Marshal(config{Addr: srv.Listener.Addr().String(), Token: "tok-uno", DeviceID: "dev-uno", Player: "Fede"})
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	world := filepath.Join(t.TempDir(), "mondo")
	c, _, e := cli(t, context.Background(), "run", "--config", cfg, "--world", world)
	if c != 1 || !strings.Contains(e, "non puoi giocare adesso") {
		t.Fatalf("code=%d err=%q", c, e)
	}
	if _, err := os.Stat(world); err == nil {
		t.Error("il mondo è stato toccato senza lease")
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"boh"}, {"enroll"}, {"run"}} {
		if c, _, _ := cli(t, context.Background(), args...); c != 2 {
			t.Errorf("%v: code=%d, atteso 2", args, c)
		}
	}
	if c, _, _ := cli(t, context.Background(), "help"); c != 0 {
		t.Error("help deve uscire con 0")
	}
	_ = io.Discard
}
