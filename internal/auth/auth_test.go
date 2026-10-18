package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newStore(t *testing.T) (*Store, *clock, string) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "state", "devices.json")
	s, err := Open(path, clk)
	if err != nil {
		t.Fatal(err)
	}
	return s, clk, path
}

func enroll(t *testing.T, s *Store, player string) (Device, string) {
	t.Helper()
	secret, err := s.CreateInvite(player)
	if err != nil {
		t.Fatal(err)
	}
	dev, token, err := s.Redeem(secret, "PC di prova")
	if err != nil {
		t.Fatal(err)
	}
	return dev, token
}

func TestInviteThenAuthenticate(t *testing.T) {
	s, _, _ := newStore(t)
	dev, token := enroll(t, s, "Fede")
	if dev.Player != "Fede" || dev.ID == "" || dev.TokenHash != "" {
		t.Fatalf("dispositivo inatteso: %+v", dev)
	}
	got, err := s.Authenticate(token)
	if err != nil || got.ID != dev.ID || got.Player != "Fede" {
		t.Fatalf("authenticate: %+v, %v", got, err)
	}
	for _, bad := range []string{"", "mcs_sbagliato", token + "x", strings.ToUpper(token)} {
		if _, err := s.Authenticate(bad); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("token %q: atteso ErrUnauthorized, ottenuto %v", bad, err)
		}
	}
}

func TestInviteIsSingleUse(t *testing.T) {
	s, _, _ := newStore(t)
	secret, _ := s.CreateInvite("Fede")
	if _, _, err := s.Redeem(secret, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Redeem(secret, ""); !errors.Is(err, ErrBadInvite) {
		t.Fatalf("atteso ErrBadInvite, ottenuto %v", err)
	}
}

func TestInviteUsedByOnlyOneOfManyAtOnce(t *testing.T) {
	s, _, _ := newStore(t)
	secret, _ := s.CreateInvite("Fede")
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.Redeem(secret, ""); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 || len(s.Devices()) != 1 {
		t.Fatalf("vincitori = %d, dispositivi = %d, attesi 1 e 1", wins, len(s.Devices()))
	}
}

func TestInviteExpiresAfter24Hours(t *testing.T) {
	s, clk, _ := newStore(t)
	secret, _ := s.CreateInvite("Fede")
	clk.Advance(InviteTTL - time.Minute)
	if _, _, err := s.Redeem(secret, ""); err != nil {
		t.Fatalf("a 23h59 l'invito doveva valere: %v", err)
	}
	secret2, _ := s.CreateInvite("Ale")
	clk.Advance(InviteTTL)
	if _, _, err := s.Redeem(secret2, ""); !errors.Is(err, ErrBadInvite) {
		t.Fatalf("a 24h l'invito doveva essere scaduto, ottenuto %v", err)
	}
}

func TestUnknownInviteRejected(t *testing.T) {
	s, _, _ := newStore(t)
	for _, secret := range []string{"", "inv_inventato"} {
		if _, _, err := s.Redeem(secret, ""); !errors.Is(err, ErrBadInvite) {
			t.Fatalf("segreto %q: atteso ErrBadInvite, ottenuto %v", secret, err)
		}
	}
}

func TestRevokeStopsOnlyThatDevice(t *testing.T) {
	s, _, _ := newStore(t)
	d1, t1 := enroll(t, s, "Fede")
	_, t2 := enroll(t, s, "Fede")
	if err := s.Revoke(d1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(t1); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("il token revocato doveva smettere di funzionare, ottenuto %v", err)
	}
	if _, err := s.Authenticate(t2); err != nil {
		t.Fatalf("l'altro dispositivo doveva continuare a funzionare: %v", err)
	}
	if err := s.Revoke("dev-inesistente"); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("atteso ErrUnknownDevice, ottenuto %v", err)
	}
}

func TestHasPlayer(t *testing.T) {
	s, _, _ := newStore(t)
	if s.HasPlayer("Fede") {
		t.Fatal("nessun dispositivo: il giocatore non esiste")
	}
	d, _ := enroll(t, s, "Fede")
	if !s.HasPlayer("Fede") || s.HasPlayer("Ale") {
		t.Fatal("HasPlayer sbagliato")
	}
	_ = s.Revoke(d.ID)
	if s.HasPlayer("Fede") {
		t.Fatal("senza dispositivi attivi il giocatore non conta")
	}
}

func TestPlayerNamesAreValidated(t *testing.T) {
	s, _, _ := newStore(t)
	for _, bad := range []string{"", "ab", "nome troppo lungo!!", "a/b", "../x", "Fe de", "Fede\n", `Fe"de`} {
		if _, err := s.CreateInvite(bad); !errors.Is(err, ErrBadPlayer) {
			t.Fatalf("nome %q: atteso ErrBadPlayer, ottenuto %v", bad, err)
		}
	}
	if _, err := s.CreateInvite("Fede_09"); err != nil {
		t.Fatal(err)
	}
}

func TestStateSurvivesRestartAndHoldsNoPlainSecrets(t *testing.T) {
	s, clk, path := newStore(t)
	dev, token := enroll(t, s, "Fede")
	pending, _ := s.CreateInvite("Ale")

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), pending) {
		t.Fatal("sul disco non devono esserci token né segreti in chiaro")
	}

	s2, err := Open(path, clk) // riavvio
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s2.Authenticate(token); err != nil || got.ID != dev.ID {
		t.Fatalf("il token doveva funzionare dopo il riavvio: %+v, %v", got, err)
	}
	if _, _, err := s2.Redeem(pending, ""); err != nil {
		t.Fatalf("l'invito in attesa doveva funzionare dopo il riavvio: %v", err)
	}
}

func TestRevokedStaysRevokedAfterRestart(t *testing.T) {
	s, clk, path := newStore(t)
	d, token := enroll(t, s, "Fede")
	_ = s.Revoke(d.ID)
	s2, _ := Open(path, clk)
	if _, err := s2.Authenticate(token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("atteso ErrUnauthorized, ottenuto %v", err)
	}
}

func TestCorruptFileRefusesToStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte(`{"format":1,"devices":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, &clock{}); err == nil {
		t.Fatal("un file rovinato non deve diventare un elenco vuoto")
	}
}

func TestFailedSaveChangesNothing(t *testing.T) {
	s, _, _ := newStore(t)
	secret, _ := s.CreateInvite("Fede")
	// la cartella dello stato diventa un file: i salvataggi falliscono
	dir := filepath.Dir(s.path)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Redeem(secret, ""); err == nil || errors.Is(err, ErrBadInvite) {
		t.Fatalf("atteso un errore di salvataggio, ottenuto %v", err)
	}
	if len(s.Devices()) != 0 {
		t.Fatal("senza salvataggio il dispositivo non deve esistere")
	}
	// l'invito non è stato consumato
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Redeem(secret, ""); err != nil {
		t.Fatalf("l'invito doveva essere ancora valido: %v", err)
	}
}

func TestLabelIsCleaned(t *testing.T) {
	s, _, _ := newStore(t)
	secret, _ := s.CreateInvite("Fede")
	dev, _, err := s.Redeem(secret, "  PC\x00 di\nFede "+strings.Repeat("x", 100))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(dev.Label, "\x00\n") || len([]rune(dev.Label)) > 40 {
		t.Fatalf("etichetta non ripulita: %q", dev.Label)
	}
}
