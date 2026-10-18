package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mcsync/internal/api"
	"mcsync/internal/archive"
	"mcsync/internal/tarutil"
)

var lim = tarutil.Limits{MaxFiles: 1000, MaxBytes: 1 << 30}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func TestEnrollUsesInviteAddressAndNoToken(t *testing.T) {
	var gotAuth, gotProto string
	var req api.EnrollRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotProto = r.Header.Get("Authorization"), r.Header.Get(api.HeaderProtocol)
		if r.URL.Path != api.PathEnroll || r.Method != http.MethodPost {
			t.Errorf("richiesta inattesa: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		writeJSON(w, 200, api.EnrollResponse{DeviceID: "dev-1", Player: "Fede", Token: "tok"})
	}))
	defer srv.Close()
	code := api.EncodeInviteCode(srv.Listener.Addr().String(), "segreto")
	addr, resp, err := Enroll(context.Background(), code, "pc-fede")
	if err != nil {
		t.Fatal(err)
	}
	if addr != srv.Listener.Addr().String() || resp.Token != "tok" || resp.Player != "Fede" {
		t.Errorf("addr=%q resp=%+v", addr, resp)
	}
	if gotAuth != "" {
		t.Errorf("l'iscrizione non deve portare Authorization: %q", gotAuth)
	}
	if gotProto != strconv.Itoa(api.ProtocolVersion) {
		t.Errorf("intestazione protocollo = %q", gotProto)
	}
	if req.Invite != "segreto" || req.Label != "pc-fede" {
		t.Errorf("corpo = %+v", req)
	}
}

func TestEnrollRejectsGarbageCodeWithoutNetwork(t *testing.T) {
	if _, _, err := Enroll(context.Background(), "ciao", "x"); err == nil {
		t.Fatal("atteso un errore")
	}
}

func TestTokenIsSentAndNeverInErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer SEGRETISSIMO" {
			writeJSON(w, 401, api.ErrorBody{Code: api.CodeUnauthorized, Message: "no"})
			return
		}
		w.Header().Set(api.HeaderRequestID, "r-1")
		writeJSON(w, 409, api.ErrorBody{Code: api.CodeLeaseHeld, Message: "gioca Marco"})
	}))
	defer srv.Close()
	c := New(srv.Listener.Addr().String(), "SEGRETISSIMO")
	_, err := c.Acquire(context.Background())
	if !IsCode(err, api.CodeLeaseHeld) {
		t.Fatalf("errore = %v", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.HTTPStatus != 409 || ae.RequestID != "r-1" {
		t.Errorf("APIError = %+v", ae)
	}
	if strings.Contains(err.Error(), "SEGRETISSIMO") {
		t.Error("il token è finito nel messaggio d'errore")
	}
	// rete morta: l'errore non deve contenere il token nemmeno qui
	srv.Close()
	_, err = c.Status(context.Background())
	if err == nil || strings.Contains(err.Error(), "SEGRETISSIMO") {
		t.Errorf("errore di rete = %v", err)
	}
	if IsCode(err, api.CodeLeaseHeld) {
		t.Error("un errore di rete non è un errore del servizio")
	}
}

func TestBusyCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		writeJSON(w, 503, api.ErrorBody{Code: api.CodeBusy, Message: "occupato"})
	}))
	defer srv.Close()
	_, err := New(srv.Listener.Addr().String(), "t").Status(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.RetryAfter != 7*time.Second {
		t.Fatalf("errore = %v", err)
	}
}

func TestNonJSONErrorBecomesInternal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>proxy</html>", 502)
	}))
	defer srv.Close()
	_, err := New(srv.Listener.Addr().String(), "t").Status(context.Background())
	if !IsCode(err, api.CodeInternal) {
		t.Fatalf("errore = %v", err)
	}
}

func makeWorld(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for n, c := range map[string]string{"level.dat": "lv", "region/r.0.0.mca": "abc", "session.lock": "no"} {
		p := filepath.Join(d, n)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// storeServer tiene l'ultimo upload e lo rimanda al download.
func storeServer(t *testing.T, mutate func([]byte) []byte) (*httptest.Server, *[]byte, *string) {
	t.Helper()
	var stored []byte
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case api.PathUpload:
			query = r.URL.RawQuery
			var buf bytes.Buffer
			if _, err := buf.ReadFrom(r.Body); err != nil {
				return
			}
			stored = buf.Bytes()
			writeJSON(w, 200, api.UploadResponse{Version: api.Version{Number: 4}, Files: 2})
		case api.PathDownload:
			b := stored
			if mutate != nil {
				b = mutate(append([]byte(nil), stored...))
			}
			w.Header().Set(api.HeaderVersion, "4")
			_, _ = w.Write(b)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &stored, &query
}

func TestUploadThenDownloadRoundTrip(t *testing.T) {
	srv, _, query := storeServer(t, nil)
	c := New(srv.Listener.Addr().String(), "t")
	world := makeWorld(t)
	resp, err := c.Upload(context.Background(), 9, 3, world, archive.Excluded)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Version.Number != 4 {
		t.Errorf("versione = %d", resp.Version.Number)
	}
	if !strings.Contains(*query, "generation=9") || !strings.Contains(*query, "parent=3") {
		t.Errorf("query = %q", *query)
	}
	dst := filepath.Join(t.TempDir(), "scaricato")
	v, err := c.Download(context.Background(), 0, dst, lim)
	if err != nil || v != 4 {
		t.Fatalf("download: v=%d err=%v", v, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "region", "r.0.0.mca")); string(b) != "abc" {
		t.Errorf("contenuto = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dst, "session.lock")); err == nil {
		t.Error("session.lock non doveva viaggiare")
	}
}

func TestDownloadRejectsTruncatedAndCorruptStreams(t *testing.T) {
	world := makeWorld(t)
	for name, mut := range map[string]func([]byte) []byte{
		"troncato":      func(b []byte) []byte { return b[:len(b)/2] },
		"senza sigillo": func(b []byte) []byte { return b[:len(b)-44] },
		"byte cambiato": func(b []byte) []byte { b[600] ^= 0xff; return b },
	} {
		t.Run(name, func(t *testing.T) {
			srv, _, _ := storeServer(t, mut)
			c := New(srv.Listener.Addr().String(), "t")
			if _, err := c.Upload(context.Background(), 1, 0, world, archive.Excluded); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Download(context.Background(), 0, filepath.Join(t.TempDir(), "d"), lim); err == nil {
				t.Fatal("un flusso rovinato è stato accettato")
			}
		})
	}
}

func TestDownloadChecksVersionHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderVersion, "5")
	}))
	defer srv.Close()
	c := New(srv.Listener.Addr().String(), "t")
	if _, err := c.Download(context.Background(), 4, filepath.Join(t.TempDir(), "d"), lim); err == nil || !strings.Contains(err.Error(), "invece") {
		t.Fatalf("errore = %v", err)
	}
}

func TestUploadRefusedByServiceReturnsAPIErrorWithSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = readAll(r)
		writeJSON(w, 409, api.ErrorBody{Code: api.CodeStaleGeneration, Message: "non sei più host", Snapshot: "orfano-1"})
	}))
	defer srv.Close()
	c := New(srv.Listener.Addr().String(), "t")
	_, err := c.Upload(context.Background(), 1, 0, makeWorld(t), archive.Excluded)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != api.CodeStaleGeneration || ae.Snapshot != "orfano-1" {
		t.Fatalf("errore = %v", err)
	}
}

func TestUploadOfMissingDirReportsReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = readAll(r)
		writeJSON(w, 400, api.ErrorBody{Code: api.CodeBadArchive, Message: "flusso incompleto"})
	}))
	defer srv.Close()
	c := New(srv.Listener.Addr().String(), "t")
	_, err := c.Upload(context.Background(), 1, 0, filepath.Join(t.TempDir(), "non-esiste"), archive.Excluded)
	if err == nil {
		t.Fatal("atteso un errore")
	}
}

func readAll(r *http.Request) (int64, error) {
	var buf bytes.Buffer
	return buf.ReadFrom(r.Body)
}
