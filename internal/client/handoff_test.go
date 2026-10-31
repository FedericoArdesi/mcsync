package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mcsync/internal/api"
)

var handoffAt = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func TestRequestHandoffSendsBodylessPostWithToken(t *testing.T) {
	want := api.HandoffRequest{From: "Fede", To: "Ale", ToDevice: "dev_ale", At: handoffAt}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != api.PathHandoffRequest ||
			len(body) != 0 || r.Header.Get("Authorization") != "Bearer tok" {
			writeJSON(w, 400, api.ErrorBody{Code: api.CodeBadRequest, Message: "richiesta inattesa"})
			return
		}
		writeJSON(w, 200, want)
	}))
	defer srv.Close()

	got, err := New(srv.Listener.Addr().String(), "tok").RequestHandoff(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("risposta = %+v, attesa %+v", got, want)
	}
}

func TestRequestHandoffServiceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 409, api.ErrorBody{Code: api.CodeHandoffPending, Message: "già una richiesta"})
	}))
	defer srv.Close()

	_, err := New(srv.Listener.Addr().String(), "tok").RequestHandoff(context.Background())
	if !IsCode(err, api.CodeHandoffPending) {
		t.Fatalf("errore = %v", err)
	}
}

func TestCancelHandoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != api.PathHandoffCancel ||
			len(body) != 0 || r.Header.Get("Authorization") != "Bearer tok" {
			writeJSON(w, 400, api.ErrorBody{Code: api.CodeBadRequest, Message: "richiesta inattesa"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := New(srv.Listener.Addr().String(), "tok").CancelHandoff(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCancelHandoffServiceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, api.ErrorBody{Code: api.CodeNotRequester, Message: "non è tua"})
	}))
	defer srv.Close()

	err := New(srv.Listener.Addr().String(), "tok").CancelHandoff(context.Background())
	if !IsCode(err, api.CodeNotRequester) {
		t.Fatalf("errore = %v", err)
	}
}

func TestHeartbeatAndStatusCarryHandoff(t *testing.T) {
	h := &api.HandoffRequest{From: "Fede", To: "Ale", ToDevice: "dev_ale", At: handoffAt}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case api.PathHeartbeat:
			writeJSON(w, 200, api.Lease{Player: "Fede", Generation: 1, Handoff: h})
		case api.PathStatus:
			writeJSON(w, 200, api.Status{Handoff: h})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.Listener.Addr().String(), "tok")

	l, err := c.Heartbeat(context.Background(), 1)
	if err != nil || l.Handoff == nil || *l.Handoff != *h {
		t.Fatalf("heartbeat: %v %+v", err, l.Handoff)
	}
	st, err := c.Status(context.Background())
	if err != nil || st.Handoff == nil || *st.Handoff != *h {
		t.Fatalf("status: %v %+v", err, st.Handoff)
	}
}
