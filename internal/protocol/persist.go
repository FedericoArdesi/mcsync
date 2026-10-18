package protocol

import (
	"errors"
	"fmt"
	"time"
)

// StateFormat è la versione del formato dello stato salvato. Un file con un
// formato diverso non si carica: meglio fermarsi che indovinare.
const StateFormat = 1

var (
	// ErrPersist: la modifica non è stata applicata perché lo stato non si è
	// potuto salvare. In memoria non è cambiato niente.
	ErrPersist = errors.New("salvataggio dello stato fallito")
	// ErrBadState: lo stato salvato è illeggibile o incoerente. Non si riparte
	// da zero in silenzio: la generazione tornerebbe indietro e un vecchio
	// host potrebbe di nuovo passare i controlli.
	ErrBadState = errors.New("stato salvato non valido")
)

// Saver salva lo stato completo. Deve essere atomico: o lo stato nuovo è
// salvato per intero, o resta quello vecchio. Viene chiamato con il lock del
// coordinatore preso, quindi due salvataggi non si sovrappongono mai.
type Saver interface {
	Save(State) error
}

// State è tutto ciò che il coordinatore deve ricordare dopo un riavvio.
// Non contiene l'orologio né lo spazio libero: arrivano da fuori.
type State struct {
	Format      int           `json:"format"`
	Generation  uint64        `json:"generation"`
	Pointer     uint64        `json:"pointer"`
	Lease       *Lease        `json:"lease,omitempty"`
	Reservation *Reservation  `json:"reservation,omitempty"`
	Versions    []Version     `json:"versions"`
	Orphans     []OrphanState `json:"orphans"`
}

// OrphanState è un Orphan nella forma salvabile: il motivo del rifiuto è il
// testo dell'errore, e al caricamento torna l'errore noto corrispondente.
type OrphanState struct {
	Request CommitRequest `json:"request"`
	Reason  string        `json:"reason"`
	At      time.Time     `json:"at"`
}

// knownReasons sono gli errori che possono finire in un orfano.
var knownReasons = []error{
	ErrHeld, ErrReserved, ErrNoLease, ErrLeaseExpired, ErrStaleGeneration,
	ErrParentMismatch, ErrNoSpace, ErrBlocksIncomplete,
}

func reasonFromText(s string) error {
	for _, e := range knownReasons {
		if e.Error() == s {
			return e
		}
	}
	return errors.New(s)
}

// NewPersistent crea un coordinatore che salva lo stato con saver a ogni
// modifica. Se st non è nil, riparte da quello stato (dopo averlo controllato).
// Una lease che nel frattempo è scaduta resta scaduta, e la generazione non
// torna mai indietro.
func NewPersistent(clock Clock, freeBytes func() int64, saver Saver, st *State) (*Coordinator, error) {
	c := &Coordinator{clock: clock, freeBytes: freeBytes, saver: saver}
	if st == nil {
		return c, nil
	}
	if err := st.validate(); err != nil {
		return nil, err
	}
	c.generation = st.Generation
	c.pointer = st.Pointer
	if st.Lease != nil {
		l := *st.Lease
		c.lease = &l
	}
	if st.Reservation != nil {
		r := *st.Reservation
		c.reservation = &r
	}
	c.versions = append([]Version(nil), st.Versions...)
	for _, o := range st.Orphans {
		c.orphans = append(c.orphans, Orphan{Request: o.Request, Reason: reasonFromText(o.Reason), At: o.At})
	}
	return c, nil
}

// State restituisce una copia dello stato attuale.
func (c *Coordinator) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateLocked()
}

func (c *Coordinator) stateLocked() State {
	st := State{
		Format:     StateFormat,
		Generation: c.generation,
		Pointer:    c.pointer,
		Versions:   append([]Version{}, c.versions...),
		Orphans:    make([]OrphanState, 0, len(c.orphans)),
	}
	if c.lease != nil {
		l := *c.lease
		st.Lease = &l
	}
	if c.reservation != nil {
		r := *c.reservation
		st.Reservation = &r
	}
	for _, o := range c.orphans {
		st.Orphans = append(st.Orphans, OrphanState{Request: o.Request, Reason: o.Reason.Error(), At: o.At})
	}
	return st
}

func (s State) validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrBadState, fmt.Sprintf(format, a...))
	}
	if s.Format != StateFormat {
		return bad("formato %d, questa versione legge il formato %d", s.Format, StateFormat)
	}
	for i, v := range s.Versions {
		if v.Number != uint64(i)+1 {
			return bad("la versione in posizione %d ha numero %d", i+1, v.Number)
		}
		if v.Parent >= v.Number {
			return bad("la versione %d ha come base la %d", v.Number, v.Parent)
		}
		if v.Generation > s.Generation {
			return bad("la versione %d ha generazione %d, più alta dell'ultima assegnata (%d)", v.Number, v.Generation, s.Generation)
		}
	}
	if s.Pointer > uint64(len(s.Versions)) {
		return bad("il puntatore è %d ma le versioni sono %d", s.Pointer, len(s.Versions))
	}
	if s.Lease != nil && (s.Lease.Generation == 0 || s.Lease.Generation > s.Generation) {
		return bad("la lease ha generazione %d, ultima assegnata %d", s.Lease.Generation, s.Generation)
	}
	if s.Reservation != nil && s.Reservation.For == "" {
		return bad("riserva senza destinatario")
	}
	return nil
}

// undo ricorda com'era lo stato prima di una modifica, per annullarla se il
// salvataggio fallisce. Versioni e orfani si possono solo aggiungere in coda,
// quindi basta ricordarne la lunghezza.
type undo struct {
	generation, pointer uint64
	lease               *Lease
	reservation         *Reservation
	versions, orphans   int
}

func (c *Coordinator) markLocked() undo {
	u := undo{generation: c.generation, pointer: c.pointer, versions: len(c.versions), orphans: len(c.orphans)}
	if c.lease != nil {
		l := *c.lease
		u.lease = &l
	}
	if c.reservation != nil {
		r := *c.reservation
		u.reservation = &r
	}
	return u
}

func (c *Coordinator) rollbackLocked(u undo) {
	c.generation, c.pointer = u.generation, u.pointer
	c.lease, c.reservation = u.lease, u.reservation
	c.versions = c.versions[:u.versions]
	c.orphans = c.orphans[:u.orphans]
}

// persistLocked salva lo stato. Se non ci riesce annulla la modifica fatta dopo
// markLocked, così quello che il coordinatore ha in memoria è sempre quello
// che c'è sul disco.
func (c *Coordinator) persistLocked(u undo) error {
	if c.saver == nil {
		return nil
	}
	if err := c.saver.Save(c.stateLocked()); err != nil {
		c.rollbackLocked(u)
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}
