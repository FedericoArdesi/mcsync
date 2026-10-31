// Package api definisce il linguaggio tra i client (l'agente sui PC) e il
// servizio sul laptop: percorsi, intestazioni, codici di errore e forma dei
// messaggi JSON. Non contiene logica. Lo usano sia il server sia, più avanti,
// l'agente, così i due lati non possono scostarsi.
//
// Tutte le richieste portano l'intestazione X-Mcsync-Protocol con la versione
// del protocollo (6.10): un client troppo vecchio o troppo nuovo viene rifiutato
// con un messaggio chiaro, senza toccare il mondo. Tutte le richieste tranne
// l'iscrizione portano il token del dispositivo: Authorization: Bearer <token>.
//
// L'identità (giocatore e dispositivo) si ricava sempre dal token, mai dal
// corpo della richiesta: un client non può parlare a nome di un altro.
package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ProtocolVersion è la versione del protocollo di rete.
const ProtocolVersion = 1

const (
	HeaderProtocol  = "X-Mcsync-Protocol"
	HeaderVersion   = "X-Mcsync-Version"    // nella risposta del download: numero della versione scaricata
	HeaderRequestID = "X-Mcsync-Request-Id" // in ogni risposta: serve a ritrovare la richiesta nei log
)

const (
	PathEnroll         = "/v1/enroll"          // POST, senza token
	PathStatus         = "/v1/status"          // GET
	PathAcquire        = "/v1/lease/acquire"   // POST
	PathHeartbeat      = "/v1/lease/heartbeat" // POST
	PathRelease        = "/v1/lease/release"   // POST
	PathUpload         = "/v1/upload"          // POST, corpo: flusso di tarutil.Pack
	PathDownload       = "/v1/download"        // GET, risposta: flusso di tarutil.Pack
	PathHandoffRequest = "/v1/handoff/request" // POST, senza corpo: chiede di diventare host
	PathHandoffCancel  = "/v1/handoff/cancel"  // POST, senza corpo: ritira la richiesta
)

// Parametri dell'indirizzo.
const (
	QueryGeneration = "generation" // upload: generazione della lease dell'host
	QueryParent     = "parent"     // upload: versione su cui si basa (0 = nessuna)
	QueryVersion    = "version"    // download: versione voluta (se manca, la corrente)
)

// Codici di errore (campo "code" di ErrorBody). Il codice è per le macchine e
// non cambia; il messaggio è per le persone e può cambiare.
const (
	CodeBadRequest      = "bad_request"
	CodeUnauthorized    = "unauthorized"
	CodeBadInvite       = "bad_invite"
	CodeProtocol        = "protocol_version"
	CodeLeaseHeld       = "lease_held"
	CodeLeaseReserved   = "lease_reserved"
	CodeNoLease         = "no_lease"
	CodeLeaseExpired    = "lease_expired"
	CodeStaleGeneration = "stale_generation"
	CodeParentMismatch  = "parent_mismatch"
	CodeAlreadyHost     = "already_host"
	CodeHandoffPending  = "handoff_pending"
	CodeNoHandoff       = "no_handoff"
	CodeNotRequester    = "not_requester"
	CodeNoSpace         = "no_space"
	CodeNoVersion       = "no_version" // non esiste ancora nessuna versione ufficiale
	CodeUnknownVersion  = "unknown_version"
	CodeBadArchive      = "bad_archive"
	CodeTooLarge        = "too_large"
	CodeBusy            = "busy"
	CodePersist         = "persist_failed"
	CodeArchive         = "archive_error" // copia mancante o rovinata nell'archivio
	CodeInternal        = "internal"
)

// ErrorBody è il corpo di ogni risposta di errore.
type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	// Snapshot, se presente, dice che il contenuto caricato è stato comunque
	// conservato nell'archivio come orfano (commit rifiutato): non si perde.
	Snapshot string `json:"snapshot,omitempty"`
}

func (e ErrorBody) Error() string { return e.Code + ": " + e.Message }

type EnrollRequest struct {
	Invite string `json:"invite"`
	Label  string `json:"label"`
}

type EnrollResponse struct {
	DeviceID string `json:"device_id"`
	Player   string `json:"player"`
	// Token si vede una sola volta: il servizio conserva solo il suo hash.
	Token string `json:"token"`
}

type Identity struct {
	Player string `json:"player"`
	Device string `json:"device"`
}

// Lease è la concessione che dice chi è host. La scadenza è secondo l'orologio
// del laptop; ExpiresInSeconds è lo stesso dato senza dipendere dall'orologio
// del client (principio 4: il tempo lo decide un solo orologio).
type Lease struct {
	Player           string    `json:"player"`
	Device           string    `json:"device"`
	Generation       uint64    `json:"generation"`
	ExpiresAt        time.Time `json:"expires_at"`
	ExpiresInSeconds int       `json:"expires_in_seconds"`
	// Handoff c'è solo nella risposta al heartbeat, se qualcuno ha chiesto di
	// diventare host: così l'host lo vede senza chiamate in più.
	Handoff *HandoffRequest `json:"handoff,omitempty"`
}

type Reservation struct {
	For              string    `json:"for"`
	Until            time.Time `json:"until"`
	ExpiresInSeconds int       `json:"expires_in_seconds"`
}

type Version struct {
	Number     uint64    `json:"number"`
	Parent     uint64    `json:"parent"`
	Author     string    `json:"author"`
	Device     string    `json:"device"`
	Generation uint64    `json:"generation"`
	CreatedAt  time.Time `json:"created_at"`
}

// HandoffRequest è la richiesta di passaggio di host in corso (6.7): un
// giocatore chiede di diventare host mentre un altro ospita.
type HandoffRequest struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	ToDevice string    `json:"to_device"`
	At       time.Time `json:"at"`
}

type Status struct {
	Protocol   int       `json:"protocol"`
	ServerTime time.Time `json:"server_time"`
	You        Identity  `json:"you"`
	// Pointer è la versione ufficiale corrente (0 = nessuna).
	Pointer uint64   `json:"pointer"`
	Current *Version `json:"current,omitempty"`
	// Lease è la lease in corso; assente se libera o scaduta.
	Lease       *Lease       `json:"lease,omitempty"`
	Reservation *Reservation `json:"reservation,omitempty"`
	// Handoff è la richiesta di passaggio in corso, se c'è.
	Handoff *HandoffRequest `json:"handoff,omitempty"`
}

// HeartbeatRequest e ReleaseRequest portano solo la generazione: giocatore e
// dispositivo si ricavano dal token.
type HeartbeatRequest struct {
	Generation uint64 `json:"generation"`
}

type ReleaseRequest struct {
	Generation uint64 `json:"generation"`
	// HandoffTo, se non è vuoto, riserva la lease a quel giocatore per 3 minuti.
	HandoffTo string `json:"handoff_to,omitempty"`
}

type UploadResponse struct {
	Version    Version `json:"version"`
	Snapshot   string  `json:"snapshot"`
	AddedBytes int64   `json:"added_bytes"`
	TotalBytes int64   `json:"total_bytes"`
	Files      int     `json:"files"`
}

// Codice di invito (8.3): è quello che l'admin manda alla persona. Contiene
// l'indirizzo del servizio e il segreto monouso; con questo l'agente si iscrive
// e riceve il suo token personale. Il codice è un segreto: va mandato solo alla
// persona interessata.
const inviteCodePrefix = "mcs1."

type inviteCode struct {
	Addr   string `json:"addr"`
	Invite string `json:"invite"`
}

// EncodeInviteCode costruisce il codice di invito da indirizzo e segreto.
func EncodeInviteCode(addr, secret string) string {
	b, _ := json.Marshal(inviteCode{Addr: addr, Invite: secret})
	return inviteCodePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// DecodeInviteCode legge un codice di invito.
func DecodeInviteCode(code string) (addr, secret string, err error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(code), inviteCodePrefix)
	if !ok {
		return "", "", errors.New("codice di invito non riconosciuto")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", "", errors.New("codice di invito non riconosciuto")
	}
	var ic inviteCode
	if err := json.Unmarshal(b, &ic); err != nil || ic.Addr == "" || ic.Invite == "" {
		return "", "", errors.New("codice di invito non riconosciuto")
	}
	return ic.Addr, ic.Invite, nil
}
