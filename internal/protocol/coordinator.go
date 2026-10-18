// Package protocol contiene le regole del protocollo di mcsync (sezione 6 del
// documento di progetto): lease dell'host, generazione, commit sicuro e
// passaggio di host.
//
// È logica pura: niente rete, niente disco, niente Minecraft. L'unica fonte di
// tempo è l'orologio del laptop (principio 4), iniettato tramite Clock, così i
// test possono simulare scadenze e guasti senza aspettare.
package protocol

import (
	"errors"
	"sync"
	"time"
)

const (
	// LeaseTTL è la durata della lease senza heartbeat (6.1) [DA CONFERMARE].
	LeaseTTL = 10 * time.Minute
	// HandoffReservation è per quanto la lease resta riservata al nuovo host
	// durante un passaggio (6.7).
	HandoffReservation = 3 * time.Minute
)

var (
	ErrHeld             = errors.New("lease occupata da un altro giocatore o dispositivo")
	ErrReserved         = errors.New("lease riservata a un altro giocatore per un passaggio di host")
	ErrNoLease          = errors.New("nessuna lease in corso")
	ErrLeaseExpired     = errors.New("lease scaduta")
	ErrStaleGeneration  = errors.New("generazione, giocatore o dispositivo non corrispondono alla lease in corso")
	ErrParentMismatch   = errors.New("la versione base non coincide con il puntatore corrente")
	ErrNoSpace          = errors.New("spazio insufficiente sul laptop")
	ErrBlocksIncomplete = errors.New("blocchi non arrivati per intero o hash non corrispondenti")
)

// Clock è l'unica fonte di tempo del protocollo.
type Clock interface{ Now() time.Time }

// SystemClock usa l'orologio di sistema (quello del laptop, in produzione).
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// Lease è la concessione a tempo che dice chi è host.
type Lease struct {
	Player     string
	Device     string
	Generation uint64
	ExpiresAt  time.Time
}

// Version è una versione ufficiale del mondo (metadati essenziali, 5.2).
type Version struct {
	Number     uint64
	Parent     uint64
	Author     string
	Device     string
	Generation uint64
	CreatedAt  time.Time
}

// CommitRequest è la richiesta di rendere ufficiale un upload.
type CommitRequest struct {
	Player     string
	Device     string
	Generation uint64
	// Parent è la versione su cui si basa l'upload.
	Parent uint64
	// BlocksVerified dice che il livello di archiviazione ha controllato che i
	// blocchi siano arrivati per intero e che gli hash corrispondano.
	BlocksVerified bool
	// SizeBytes è lo spazio che la versione occuperà sul laptop.
	SizeBytes int64
}

// Orphan è un commit rifiutato: il contenuto non si perde, va in orphans/ sul
// laptop e in recovery/ sul PC dell'autore (6.2).
type Orphan struct {
	Request CommitRequest
	Reason  error
	At      time.Time
}

type reservation struct {
	For   string
	Until time.Time
}

// Coordinator tiene lo stato del protocollo. È sicuro per uso concorrente.
type Coordinator struct {
	mu        sync.Mutex
	clock     Clock
	freeBytes func() int64 // spazio libero sul laptop; nil = illimitato

	generation  uint64 // ultima generazione assegnata
	lease       *Lease // nil se rilasciata o mai presa
	reservation *reservation
	pointer     uint64 // versione ufficiale corrente (0 = nessuna)
	versions    []Version
	orphans     []Orphan
}

// NewCoordinator crea un coordinatore. freeBytes può essere nil.
func NewCoordinator(clock Clock, freeBytes func() int64) *Coordinator {
	return &Coordinator{clock: clock, freeBytes: freeBytes}
}

// Acquire prova a prendere la lease. La presa è atomica: se due giocatori
// partono insieme ne vince uno solo. Ogni nuova lease incrementa la
// generazione. Se la lease è già del chiamante (stesso giocatore e stesso
// dispositivo) viene solo rinnovata, senza cambiare generazione.
func (c *Coordinator) Acquire(player, device string) (Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()

	if c.lease != nil && now.Before(c.lease.ExpiresAt) {
		if c.lease.Player == player && c.lease.Device == device {
			c.lease.ExpiresAt = now.Add(LeaseTTL)
			return *c.lease, nil
		}
		return Lease{}, ErrHeld
	}

	if r := c.reservation; r != nil {
		if now.Before(r.Until) && r.For != player {
			return Lease{}, ErrReserved
		}
		c.reservation = nil // consumata o scaduta
	}

	c.generation++
	l := Lease{
		Player:     player,
		Device:     device,
		Generation: c.generation,
		ExpiresAt:  now.Add(LeaseTTL),
	}
	c.lease = &l
	return l, nil
}

// Heartbeat rinnova la lease. Una lease già scaduta non si rinnova: bisogna
// riprenderla con Acquire (e prenderà una generazione nuova).
func (c *Coordinator) Heartbeat(player, device string, gen uint64) (Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkHolderLocked(player, device, gen); err != nil {
		return Lease{}, err
	}
	c.lease.ExpiresAt = c.clock.Now().Add(LeaseTTL)
	return *c.lease, nil
}

// Release rilascia la lease. Se handoffTo non è vuoto, la lease resta
// riservata a quel giocatore per HandoffReservation. Va chiamata solo dopo la
// conferma del commit finale (6.6).
func (c *Coordinator) Release(player, device string, gen uint64, handoffTo string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lease == nil {
		return ErrNoLease
	}
	if c.lease.Generation != gen || c.lease.Player != player || c.lease.Device != device {
		return ErrStaleGeneration
	}
	c.lease = nil
	if handoffTo != "" {
		c.reservation = &reservation{For: handoffTo, Until: c.clock.Now().Add(HandoffReservation)}
	}
	return nil
}

// Commit rende ufficiale un upload se supera i controlli di 6.2:
// blocchi integri, generazione corrente, base uguale al puntatore (compare-and-swap),
// spazio sufficiente. Un commit rifiutato (tranne per blocchi incompleti, che
// riprendono) viene registrato come orfano.
func (c *Coordinator) Commit(req CommitRequest) (Version, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()

	if !req.BlocksVerified {
		return Version{}, ErrBlocksIncomplete
	}

	reject := func(err error) (Version, error) {
		c.orphans = append(c.orphans, Orphan{Request: req, Reason: err, At: now})
		return Version{}, err
	}

	if err := c.checkHolderLocked(req.Player, req.Device, req.Generation); err != nil {
		return reject(err)
	}
	if req.Parent != c.pointer {
		return reject(ErrParentMismatch)
	}
	if c.freeBytes != nil && c.freeBytes() < req.SizeBytes {
		return reject(ErrNoSpace)
	}

	v := Version{
		Number:     uint64(len(c.versions)) + 1,
		Parent:     req.Parent,
		Author:     req.Player,
		Device:     req.Device,
		Generation: req.Generation,
		CreatedAt:  now,
	}
	c.versions = append(c.versions, v)
	c.pointer = v.Number // passo atomico: il puntatore cambia sotto lo stesso lock
	return v, nil
}

// Pointer restituisce la versione ufficiale corrente (0 = nessuna).
func (c *Coordinator) Pointer() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pointer
}

// CurrentLease restituisce la lease in corso, se esiste e non è scaduta.
func (c *Coordinator) CurrentLease() (Lease, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lease == nil || !c.clock.Now().Before(c.lease.ExpiresAt) {
		return Lease{}, false
	}
	return *c.lease, true
}

// Versions restituisce una copia dell'elenco delle versioni ufficiali.
func (c *Coordinator) Versions() []Version {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Version(nil), c.versions...)
}

// Orphans restituisce una copia dei commit rifiutati.
func (c *Coordinator) Orphans() []Orphan {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Orphan(nil), c.orphans...)
}

// checkHolderLocked verifica che i dati corrispondano alla lease in corso e
// che non sia scaduta. Va chiamata con il lock preso.
func (c *Coordinator) checkHolderLocked(player, device string, gen uint64) error {
	if c.lease == nil {
		return ErrNoLease
	}
	if c.lease.Generation != gen || c.lease.Player != player || c.lease.Device != device {
		return ErrStaleGeneration
	}
	if !c.clock.Now().Before(c.lease.ExpiresAt) {
		return ErrLeaseExpired
	}
	return nil
}
