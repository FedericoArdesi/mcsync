// Package auth gestisce i dispositivi e i loro token (sezione 8.2 e 8.3 del
// documento di progetto).
//
// Ogni dispositivo ha un token personale, revocabile da solo. Sul disco
// finiscono solo gli hash: chi legge il file non può usare i token. Un
// dispositivo nuovo entra con un codice di invito monouso che scade dopo 24
// ore. L'amministrazione (inviti e revoche) non passa dalla rete: si fa sul
// laptop.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/FedericoArdesi/mcsync/internal/atomicfile"
	"github.com/FedericoArdesi/mcsync/internal/protocol"
)

const (
	// InviteTTL è la durata di un codice di invito (8.3).
	InviteTTL  = 24 * time.Hour
	fileFormat = 1
)

var (
	ErrUnauthorized  = errors.New("token non valido o revocato")
	ErrBadInvite     = errors.New("codice di invito non valido, scaduto o già usato")
	ErrBadPlayer     = errors.New("nome del giocatore non valido")
	ErrUnknownDevice = errors.New("dispositivo sconosciuto")
)

var playerRe = regexp.MustCompile(`^[A-Za-z0-9_]{3,16}$`)

// ValidPlayer dice se il nome è un nome Minecraft Java possibile (3-16
// caratteri tra lettere, cifre e trattino basso). Si controlla ovunque un nome
// arrivi da fuori (8.2, "nomi validati").
func ValidPlayer(name string) bool { return playerRe.MatchString(name) }

// Device è un dispositivo autorizzato.
type Device struct {
	ID        string    `json:"id"`
	Player    string    `json:"player"`
	Label     string    `json:"label"`
	TokenHash string    `json:"token_hash,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Revoked   bool      `json:"revoked"`
}

type invite struct {
	Player     string    `json:"player"`
	SecretHash string    `json:"secret_hash"`
	ExpiresAt  time.Time `json:"expires_at"`
	Used       bool      `json:"used"`
}

type fileData struct {
	Format  int      `json:"format"`
	Devices []Device `json:"devices"`
	Invites []invite `json:"invites"`
}

// Store tiene dispositivi e inviti e li salva su disco a ogni modifica. È
// sicuro per uso concorrente.
type Store struct {
	mu    sync.Mutex
	path  string
	clock protocol.Clock
	data  fileData
}

// Open carica il file dei dispositivi (se non esiste parte vuoto). Un file
// illeggibile è un errore: meglio non partire che aprire o chiudere a caso.
func Open(path string, clock protocol.Clock) (*Store, error) {
	s := &Store{path: path, clock: clock, data: fileData{Format: fileFormat}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var d fileData
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("file dei dispositivi %s illeggibile: %w", path, err)
	}
	if d.Format != fileFormat {
		return nil, fmt.Errorf("file dei dispositivi %s: formato %d non supportato", path, d.Format)
	}
	s.data = d
	return s, nil
}

// mutate applica una modifica a una copia dei dati, la salva e solo allora la
// rende effettiva: se il salvataggio fallisce, in memoria non cambia niente.
func (s *Store) mutate(fn func(d *fileData) error) error {
	next := fileData{
		Format:  fileFormat,
		Devices: append([]Device(nil), s.data.Devices...),
		Invites: append([]invite(nil), s.data.Invites...),
	}
	if err := fn(&next); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(s.path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("salvataggio dei dispositivi fallito: %w", err)
	}
	s.data = next
	return nil
}

// CreateInvite crea un invito per un giocatore e restituisce il segreto, che
// l'admin deve far arrivare alla persona (di solito dentro il codice di invito
// con l'indirizzo del servizio). Il segreto non si può rileggere: sul disco
// c'è solo il suo hash.
func (s *Store) CreateInvite(player string) (secret string, err error) {
	if !ValidPlayer(player) {
		return "", ErrBadPlayer
	}
	secret = newSecret("inv_")
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.mutate(func(d *fileData) error {
		kept := d.Invites[:0:0]
		for _, in := range d.Invites { // si tolgono gli inviti scaduti o usati
			if !in.Used && now.Before(in.ExpiresAt) {
				kept = append(kept, in)
			}
		}
		d.Invites = append(kept, invite{Player: player, SecretHash: hashOf(secret), ExpiresAt: now.Add(InviteTTL)})
		return nil
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// Redeem usa un invito: crea il dispositivo e restituisce il suo token
// personale, che si vede una volta sola. L'invito vale una volta e scade dopo
// 24 ore.
func (s *Store) Redeem(secret, label string) (Device, string, error) {
	h := hashOf(secret)
	now := s.clock.Now()
	var dev Device
	var token string

	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.mutate(func(d *fileData) error {
		idx := -1
		for i := range d.Invites {
			if subtle.ConstantTimeCompare([]byte(d.Invites[i].SecretHash), []byte(h)) == 1 {
				idx = i
				break
			}
		}
		if idx < 0 || d.Invites[idx].Used || !now.Before(d.Invites[idx].ExpiresAt) {
			return ErrBadInvite
		}
		d.Invites[idx].Used = true
		token = newSecret("mcs_")
		dev = Device{
			ID:        "dev-" + randHex(6),
			Player:    d.Invites[idx].Player,
			Label:     cleanLabel(label),
			TokenHash: hashOf(token),
			CreatedAt: now,
		}
		d.Devices = append(d.Devices, dev)
		return nil
	})
	if err != nil {
		return Device{}, "", err
	}
	dev.TokenHash = ""
	return dev, token, nil
}

// Authenticate trova il dispositivo di un token. Un token sconosciuto o
// revocato dà ErrUnauthorized, senza dire quale dei due casi.
func (s *Store) Authenticate(token string) (Device, error) {
	h := hashOf(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.data.Devices {
		if subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(h)) == 1 {
			if d.Revoked {
				return Device{}, ErrUnauthorized
			}
			d.TokenHash = ""
			return d, nil
		}
	}
	return Device{}, ErrUnauthorized
}

// Revoke revoca un dispositivo: il suo token smette di funzionare subito.
func (s *Store) Revoke(deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutate(func(d *fileData) error {
		for i := range d.Devices {
			if d.Devices[i].ID == deviceID {
				d.Devices[i].Revoked = true
				return nil
			}
		}
		return ErrUnknownDevice
	})
}

// HasPlayer dice se il giocatore ha almeno un dispositivo non revocato.
func (s *Store) HasPlayer(player string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.data.Devices {
		if d.Player == player && !d.Revoked {
			return true
		}
	}
	return false
}

// Devices restituisce l'elenco dei dispositivi, senza gli hash dei token.
func (s *Store) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Device(nil), s.data.Devices...)
	for i := range out {
		out[i].TokenHash = ""
	}
	return out
}

func cleanLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if r := []rune(out); len(r) > 40 {
		out = string(r[:40])
	}
	return out
}

func newSecret(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("auth: generatore casuale non disponibile: " + err.Error())
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("auth: generatore casuale non disponibile: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
