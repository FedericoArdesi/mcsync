// Package protocol contiene le regole del protocollo di mcsync (sezione 6 del
// documento di progetto): lease dell'host, generazione, commit sicuro e
// passaggio di host.
//
// È logica pura: niente rete, niente disco, niente Minecraft. L'unica fonte di
// tempo è l'orologio del laptop (principio 4), iniettato tramite Clock, così i
// test possono simulare scadenze e guasti senza aspettare.
//
// Lo stato può essere salvato da un Saver (vedi persist.go): il coordinatore
// non sa dove né come, sa solo che una modifica è confermata quando il Saver
// risponde senza errore.
package protocol

import (
	"errors"
	"fmt"
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
	Player     string    `json:"player"`
	Device     string    `json:"device"`
	Generation uint64    `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Version è una versione ufficiale del mondo (metadati essenziali, 5.2).
type Version struct {
	Number     uint64    `json:"number"`
	Parent     uint64    `json:"parent"`
	Author     string    `json:"author"`
	Device     string    `json:"device"`
	Generation uint64    `json:"generation"`
	CreatedAt  time.Time `json:"created_at"`
	// Snapshot è l'identificativo della copia nell'archivio che contiene
	// il contenuto di questa versione.
	Snapshot string `json:"snapshot"`
}

// CommitRequest è la richiesta di rendere ufficiale un upload.
type CommitRequest struct {
	Player     string `json:"player"`
	Device     string `json:"device"`
	Generation uint64 `json:"generation"`
	// Parent è la versione su cui si basa l'upload.
	Parent uint64 `json:"parent"`
	// BlocksVerified dice che il livello di archiviazione ha controllato che i
	// blocchi siano arrivati per intero e che gli hash corrispondano.
	BlocksVerified bool `json:"blocks_verified"`
	// SizeBytes è lo spazio che la versione occuperà sul laptop.
	SizeBytes int64 `json:"size_bytes"`
	// Snapshot è l'identificativo della copia nell'archivio. Se il commit
	// viene rifiutato, serve a ritrovare il contenuto dell'orfano.
	Snapshot string `json:"snapshot"`
}

// Orphan è un commit rifiutato: il contenuto non si perde, va in orphans/ sul
// laptop e in recovery/ sul PC dell'autore (6.2).
type Orphan struct {
	Request CommitRequest
	Reason  error
	At      time.Time
}

// Reservation dice che la lease, appena rilasciata, è riservata a un giocatore
// fino a una certa ora (passaggio di host, 6.7).
type Reservation struct {
	For   string    `json:"for"`
	Until time.Time `json:"until"`
}

// Coordinator tiene lo stato del protocollo. È sicuro per uso concorrente.
type Coordinator struct {
	mu        sync.Mutex
	clock     Clock
	freeBytes func() int64 // spazio libero sul laptop; nil = illimitato
	saver     Saver        // nil = lo stato non viene salvato

	generation  uint64 // ultima generazione assegnata
	lease       *Lease // nil se rilasciata o mai presa
	reservation *Reservation
	handoff     *HandoffRequest // richiesta di passaggio in corso (handoff.go)
	pointer     uint64          // versione ufficiale corrente (0 = nessuna)
	versions    []Version
	orphans     []Orphan
}

// NewCoordinator crea un coordinatore che tiene lo stato solo in memoria.
// freeBytes può essere nil. Per uno stato che sopravvive ai riavvii si usa
// NewPersistent.
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
	u := c.markLocked()

	if c.lease != nil && now.Before(c.lease.ExpiresAt) {
		if c.lease.Player == player && c.lease.Device == device {
			c.lease.ExpiresAt = now.Add(LeaseTTL)
			if err := c.persistLocked(u); err != nil {
				return Lease{}, err
			}
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

	c.handoff = nil // una richiesta appartiene alla lease precedente
	c.generation++
	l := Lease{
		Player:     player,
		Device:     device,
		Generation: c.generation,
		ExpiresAt:  now.Add(LeaseTTL),
	}
	c.lease = &l
	if err := c.persistLocked(u); err != nil {
		return Lease{}, err
	}
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
	u := c.markLocked()
	c.lease.ExpiresAt = c.clock.Now().Add(LeaseTTL)
	if err := c.persistLocked(u); err != nil {
		return Lease{}, err
	}
	return *c.lease, nil
}

// Release rilascia la lease. Se handoffTo è il giocatore della richiesta di
// passaggio in corso, la lease resta riservata a lui per HandoffReservation.
// Se handoffTo è vuoto, o non coincide con il richiedente (richiesta ritirata
// o cambiata mentre l'host chiudeva), la lease si rilascia comunque, ma libera:
// la decisione la prende il coordinatore, sotto lo stesso blocco, così non
// dipende da cosa l'host ha visto per ultimo. Va chiamata solo dopo la
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
	reserve := handoffTo != "" && c.handoff != nil && c.handoff.To == handoffTo
	u := c.markLocked()
	c.lease = nil
	c.handoff = nil // con la lease finisce anche la richiesta
	if reserve {
		c.reservation = &Reservation{For: handoffTo, Until: c.clock.Now().Add(HandoffReservation)}
	}
	return c.persistLocked(u)
}

// Commit rende ufficiale un upload se supera i controlli di 6.2:
// blocchi integri, generazione corrente, base uguale al puntatore (compare-and-swap),
// spazio sufficiente. Un commit rifiutato (tranne per blocchi incompleti, che
// riprendono) viene registrato come orfano.
//
// Se lo stato non si riesce a salvare, il commit non avviene: la versione non
// esiste e il puntatore resta dov'era (l'errore contiene ErrPersist).
func (c *Coordinator) Commit(req CommitRequest) (Version, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()

	if !req.BlocksVerified {
		return Version{}, ErrBlocksIncomplete
	}

	reject := func(err error) (Version, error) {
		u := c.markLocked()
		c.orphans = append(c.orphans, Orphan{Request: req, Reason: err, At: now})
		if perr := c.persistLocked(u); perr != nil {
			// l'orfano non è stato registrato: si riportano entrambi gli errori
			return Version{}, fmt.Errorf("%w; %w", err, perr)
		}
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

	u := c.markLocked()
	v := Version{
		Number:     uint64(len(c.versions)) + 1,
		Parent:     req.Parent,
		Author:     req.Player,
		Device:     req.Device,
		Generation: req.Generation,
		CreatedAt:  now,
		Snapshot:   req.Snapshot,
	}
	c.versions = append(c.versions, v)
	c.pointer = v.Number // versione e puntatore cambiano insieme, nello stesso salvataggio
	if err := c.persistLocked(u); err != nil {
		return Version{}, err
	}
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

// Version restituisce la versione ufficiale con quel numero (da 1 in poi).
func (c *Coordinator) Version(n uint64) (Version, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n == 0 || n > uint64(len(c.versions)) {
		return Version{}, false
	}
	return c.versions[n-1], true
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
