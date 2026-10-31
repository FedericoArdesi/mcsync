package admin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/FedericoArdesi/mcsync/internal/auth"
	"github.com/FedericoArdesi/mcsync/internal/protocol"
)

func setup(t *testing.T) (*auth.Store, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("socket Unix: il servizio gira su Linux")
	}
	dir, err := os.MkdirTemp("", "mcs") // percorso corto: i socket Unix hanno un limite di lunghezza
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	store, err := auth.Open(filepath.Join(dir, "devices.json"), protocol.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "a.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, sock, Handler(store)) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve non si è fermato")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for !Running(sock) {
		if time.Now().After(deadline) {
			t.Fatal("il socket non è partito")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return store, sock
}

func TestInviteDevicesRevokeThroughSocket(t *testing.T) {
	store, sock := setup(t)
	c := NewClient(sock)

	secret, err := c.Invite("Fede")
	if err != nil || secret == "" {
		t.Fatalf("invito: %q %v", secret, err)
	}
	// lo stesso archivio in memoria del servizio vede subito l'invito
	dev, token, err := store.Redeem(secret, "PC")
	if err != nil {
		t.Fatalf("l'invito doveva valere per il servizio in funzione: %v", err)
	}
	devs, err := c.Devices()
	if err != nil || len(devs) != 1 || devs[0].ID != dev.ID || devs[0].TokenHash != "" {
		t.Fatalf("elenco: %+v %v", devs, err)
	}
	if err := c.Revoke(dev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(token); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("il token doveva essere revocato subito: %v", err)
	}
}

func TestErrorsAreMappedBack(t *testing.T) {
	_, sock := setup(t)
	c := NewClient(sock)
	if _, err := c.Invite("../x"); !errors.Is(err, auth.ErrBadPlayer) {
		t.Fatalf("atteso ErrBadPlayer, ottenuto %v", err)
	}
	if err := c.Revoke("dev-inesistente"); !errors.Is(err, auth.ErrUnknownDevice) {
		t.Fatalf("atteso ErrUnknownDevice, ottenuto %v", err)
	}
}

func TestSocketIsPrivateAndOnlyOneServiceRuns(t *testing.T) {
	_, sock := setup(t)
	st, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("il socket deve essere leggibile solo dal proprietario, è %v", st.Mode().Perm())
	}
	if err := Serve(context.Background(), sock, Handler(nil)); err == nil {
		t.Fatal("un secondo servizio sullo stesso socket doveva essere rifiutato")
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("socket Unix")
	}
	dir, _ := os.MkdirTemp("", "mcs")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "a.sock")
	if err := os.WriteFile(sock, []byte("resto di un servizio morto"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := auth.Open(filepath.Join(dir, "d.json"), protocol.SystemClock{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, sock, Handler(store)) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for !Running(sock) {
		if time.Now().After(deadline) {
			t.Fatal("il socket vecchio doveva essere sostituito")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClientWithoutServiceReportsNotRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("socket Unix")
	}
	c := NewClient(filepath.Join(os.TempDir(), "mcsync-non-esiste.sock"))
	if _, err := c.Devices(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("atteso ErrNotRunning, ottenuto %v", err)
	}
}
