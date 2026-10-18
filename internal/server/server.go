// Package server espone il servizio sul laptop come API HTTP+JSON (sezioni 4.2
// e 8.2 del documento di progetto).
//
// Scelte di sicurezza:
//
//   - il server ascolta solo sull'indirizzo indicato (quello della rete
//     privata) e rifiuta indirizzi "jolly" come 0.0.0.0 (CheckListenAddr);
//   - ogni richiesta, tranne l'iscrizione con codice di invito, porta il token
//     di un dispositivo; giocatore e dispositivo si ricavano dal token e mai dal
//     corpo della richiesta;
//   - il client può solo prendere e rilasciare la lease, caricare e scaricare il
//     mondo: non esiste nessuna operazione per cancellare versioni, cambiare la
//     retention o amministrare dispositivi (inviti e revoche si fanno sul
//     laptop, non dalla rete);
//   - il flusso del mondo viene controllato da tarutil.Unpack (nomi, tipi di
//     voce, limiti, sigillo SHA-256) prima di toccare l'archivio;
//   - gli errori interni non rivelano percorsi né testi di errore: il dettaglio
//     va nel log, insieme a un identificativo di richiesta che il client vede;
//   - token e codici di invito non finiscono mai nel log.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mcsync/internal/api"
	"mcsync/internal/archive"
	"mcsync/internal/auth"
	"mcsync/internal/protocol"
	"mcsync/internal/service"
	"mcsync/internal/tarutil"
)

const (
	defaultMaxUploadBytes = 32 << 30 // 32 GiB di contenuto: molto sopra i mondi attesi (3-10 GB)
	defaultMaxUploadFiles = 500_000
	defaultMaxTransfers   = 2
	defaultTransferIdle   = 60 * time.Second
	defaultShutdownGrace  = 30 * time.Second
	maxJSONBody           = 64 << 10
)

// Config raccoglie ciò che serve al server. Coordinator, Service e Auth sono
// obbligatori; gli altri campi hanno un valore predefinito.
type Config struct {
	Coordinator *protocol.Coordinator
	Service     *service.Service
	Auth        *auth.Store
	// Clock deve essere lo stesso del coordinatore (default: orologio di sistema).
	Clock protocol.Clock
	// WorkDir è la cartella per i file temporanei di upload e download. Il
	// suo contenuto (sottocartella tmp) viene svuotato a ogni avvio: sono resti
	// di trasferimenti interrotti dal riavvio.
	WorkDir string
	Logger  *slog.Logger

	// MaxUploadBytes e MaxUploadFiles limitano un upload (contenuto del mondo).
	MaxUploadBytes int64
	MaxUploadFiles int
	// MaxTransfers è il numero massimo di upload e download contemporanei: il
	// laptop ospita altri servizi. Oltre il limite la risposta è 503.
	MaxTransfers int
	// TransferIdle è il tempo massimo senza dati in un trasferimento prima di
	// chiuderlo: un client bloccato non tiene occupato un posto per sempre.
	TransferIdle time.Duration
	// ShutdownGrace è quanto aspettare i trasferimenti in corso allo spegnimento.
	ShutdownGrace time.Duration
}

// Server è il servizio di rete. Si usa tramite Handler, Serve o ListenAndServe.
type Server struct {
	coord    *protocol.Coordinator
	svc      *service.Service
	auth     *auth.Store
	clock    protocol.Clock
	log      *slog.Logger
	tmp      string
	maxBytes int64
	maxFiles int
	idle     time.Duration
	grace    time.Duration
	slots    chan struct{}
}

// New crea il server. Svuota la cartella temporanea: dopo un riavvio i
// trasferimenti a metà non esistono più.
func New(cfg Config) (*Server, error) {
	if cfg.Coordinator == nil || cfg.Service == nil || cfg.Auth == nil {
		return nil, errors.New("server: servono coordinatore, servizio e archivio dei dispositivi")
	}
	if cfg.WorkDir == "" {
		return nil, errors.New("server: manca la cartella di lavoro")
	}
	s := &Server{
		coord:    cfg.Coordinator,
		svc:      cfg.Service,
		auth:     cfg.Auth,
		clock:    cfg.Clock,
		log:      cfg.Logger,
		maxBytes: cfg.MaxUploadBytes,
		maxFiles: cfg.MaxUploadFiles,
		idle:     cfg.TransferIdle,
		grace:    cfg.ShutdownGrace,
	}
	if s.clock == nil {
		s.clock = protocol.SystemClock{}
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.maxBytes <= 0 {
		s.maxBytes = defaultMaxUploadBytes
	}
	if s.maxFiles <= 0 {
		s.maxFiles = defaultMaxUploadFiles
	}
	if s.idle <= 0 {
		s.idle = defaultTransferIdle
	}
	if s.grace <= 0 {
		s.grace = defaultShutdownGrace
	}
	transfers := cfg.MaxTransfers
	if transfers <= 0 {
		transfers = defaultMaxTransfers
	}
	s.slots = make(chan struct{}, transfers)

	s.tmp = filepath.Join(cfg.WorkDir, "tmp")
	if err := os.RemoveAll(s.tmp); err != nil {
		return nil, fmt.Errorf("server: pulizia di %s: %w", s.tmp, err)
	}
	if err := os.MkdirAll(s.tmp, 0o700); err != nil {
		return nil, fmt.Errorf("server: cartella temporanea: %w", err)
	}
	return s, nil
}

// InFlight è il numero di trasferimenti (upload e download) in corso.
func (s *Server) InFlight() int { return len(s.slots) }

// Handler restituisce il gestore HTTP con tutti i percorsi.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+api.PathEnroll, s.enroll)
	mux.HandleFunc("GET "+api.PathStatus, s.authed(s.status))
	mux.HandleFunc("POST "+api.PathAcquire, s.authed(s.acquire))
	mux.HandleFunc("POST "+api.PathHeartbeat, s.authed(s.heartbeat))
	mux.HandleFunc("POST "+api.PathRelease, s.authed(s.release))
	mux.HandleFunc("POST "+api.PathUpload, s.authed(s.upload))
	mux.HandleFunc("GET "+api.PathDownload, s.authed(s.download))
	return s.logging(s.protocolCheck(mux))
}

// CheckListenAddr rifiuta gli indirizzi che aprirebbero il servizio a tutte le
// interfacce (0.0.0.0, [::], porta sola) e i nomi al posto di un indirizzo IP:
// il servizio deve ascoltare solo sull'indirizzo della rete privata (8.2).
func CheckListenAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("indirizzo %q non valido (serve la forma indirizzo-ip:porta): %w", addr, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("porta %q non valida", port)
	}
	if host == "" {
		return fmt.Errorf("indirizzo %q: manca l'indirizzo IP, così il servizio ascolterebbe su tutte le interfacce", addr)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("indirizzo %q: usa un indirizzo IP, non un nome", addr)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("indirizzo %q: ascolterebbe su tutte le interfacce; indica l'indirizzo della rete privata", addr)
	}
	return nil
}

// ListenAndServe ascolta su addr (vedi CheckListenAddr) finché ctx non viene
// annullato, poi si ferma con calma.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if err := CheckListenAddr(addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.log.Info("servizio in ascolto", "addr", ln.Addr().String())
	return s.Serve(ctx, ln)
}

// Serve serve le richieste su ln finché ctx non viene annullato. Allo
// spegnimento aspetta i trasferimenti in corso (ShutdownGrace) e poi chiude.
// Restituisce nil dopo uno spegnimento pulito.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), s.grace)
		defer cancel()
		err := srv.Shutdown(shutCtx)
		if err != nil {
			_ = srv.Close()
		}
		<-errc
		return err
	}
}

// ---------------------------------------------------------------------------
// Middleware

type ctxKey struct{}

// reqInfo passa dati della richiesta tra i livelli (per il log e gli errori).
type reqInfo struct {
	id     string
	device string
	player string
}

func infoOf(ctx context.Context) *reqInfo {
	if i, ok := ctx.Value(ctxKey{}).(*reqInfo); ok {
		return i
	}
	return &reqInfo{}
}

// statusRecorder ricorda lo stato della risposta, per il log. Unwrap lascia
// funzionare http.ResponseController (scadenze di lettura e scrittura).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &reqInfo{id: randHex(6)}
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, info))
		w.Header().Set(api.HeaderRequestID, info.id)
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		defer func() {
			// Anche se la risposta viene interrotta di proposito
			// (http.ErrAbortHandler) il log si scrive, poi il panic prosegue.
			s.log.Info("richiesta",
				"id", info.id, "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "device", info.device, "player", info.player,
				"duration", time.Since(start).Round(time.Millisecond))
		}()
		next.ServeHTTP(rec, r)
	})
}

func (s *Server) protocolCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(api.HeaderProtocol) != strconv.Itoa(api.ProtocolVersion) {
			s.failMsg(w, r, http.StatusUpgradeRequired, api.CodeProtocol,
				fmt.Sprintf("versione del protocollo non supportata: il servizio usa la %d", api.ProtocolVersion), "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type authedHandler func(w http.ResponseWriter, r *http.Request, dev auth.Device)

// authed controlla il token e passa al gestore il dispositivo. Token mancante,
// sconosciuto o revocato danno la stessa risposta.
func (s *Server) authed(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r.Header.Get("Authorization"))
		if ok {
			if dev, err := s.auth.Authenticate(token); err == nil {
				info := infoOf(r.Context())
				info.device, info.player = dev.ID, dev.Player
				h(w, r, dev)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		s.failMsg(w, r, http.StatusUnauthorized, api.CodeUnauthorized, auth.ErrUnauthorized.Error(), "")
	}
}

func bearer(h string) (string, bool) {
	f := strings.Fields(h)
	if len(f) != 2 || !strings.EqualFold(f[0], "Bearer") {
		return "", false
	}
	return f[1], true
}

// ---------------------------------------------------------------------------
// Risposte e errori

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) failMsg(w http.ResponseWriter, r *http.Request, status int, code, msg, snapshot string) {
	writeJSON(w, status, api.ErrorBody{Code: code, Message: msg, RequestID: infoOf(r.Context()).id, Snapshot: snapshot})
}

// fail traduce un errore in una risposta. snapshot, se non è vuoto, è la copia
// conservata come orfano da un commit rifiutato.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error, snapshot string) {
	status, code, msg := classify(err)
	switch {
	case status >= 500:
		s.log.Error("richiesta fallita", "id", infoOf(r.Context()).id, "path", r.URL.Path, "error", err)
	case code == api.CodeBadArchive || code == api.CodeTooLarge:
		s.log.Warn("upload rifiutato", "id", infoOf(r.Context()).id, "device", infoOf(r.Context()).device, "error", err)
	}
	s.failMsg(w, r, status, code, msg, snapshot)
}

// classify sceglie stato, codice e messaggio. I messaggi sono fissi: il testo
// dell'errore vero (che può contenere percorsi) va solo nel log.
func classify(err error) (status int, code, msg string) {
	var tooBig *http.MaxBytesError
	switch {
	case errors.Is(err, protocol.ErrPersist):
		return 500, api.CodePersist, "il servizio non riesce a salvare il proprio stato: riprova tra poco"
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, protocol.ErrNoSpace):
		return 507, api.CodeNoSpace, "spazio insufficiente sul laptop"
	case errors.As(err, &tooBig), errors.Is(err, tarutil.ErrTooLarge):
		return 413, api.CodeTooLarge, "il mondo supera i limiti di dimensione del servizio"
	case errors.Is(err, tarutil.ErrBadArchive):
		return 400, api.CodeBadArchive, "l'archivio ricevuto non è valido o è incompleto"
	case errors.Is(err, protocol.ErrHeld):
		return 409, api.CodeLeaseHeld, protocol.ErrHeld.Error()
	case errors.Is(err, protocol.ErrReserved):
		return 409, api.CodeLeaseReserved, protocol.ErrReserved.Error()
	case errors.Is(err, protocol.ErrNoLease):
		return 409, api.CodeNoLease, protocol.ErrNoLease.Error()
	case errors.Is(err, protocol.ErrLeaseExpired):
		return 409, api.CodeLeaseExpired, protocol.ErrLeaseExpired.Error()
	case errors.Is(err, protocol.ErrStaleGeneration):
		return 409, api.CodeStaleGeneration, protocol.ErrStaleGeneration.Error()
	case errors.Is(err, protocol.ErrParentMismatch):
		return 409, api.CodeParentMismatch, protocol.ErrParentMismatch.Error()
	case errors.Is(err, service.ErrUnknownVersion):
		return 404, api.CodeUnknownVersion, service.ErrUnknownVersion.Error()
	case errors.Is(err, archive.ErrNotFound), errors.Is(err, archive.ErrCorrupt):
		return 500, api.CodeArchive, "la copia richiesta non è disponibile o è rovinata nell'archivio"
	default:
		return 500, api.CodeInternal, "errore interno del servizio"
	}
}

// readJSON legge un corpo JSON piccolo. Se non va, risponde da solo e
// restituisce false.
func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err := dec.Decode(v); err != nil {
		s.failMsg(w, r, http.StatusBadRequest, api.CodeBadRequest, "corpo della richiesta non valido", "")
		return false
	}
	return true
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("server: generatore casuale non disponibile: " + err.Error())
	}
	return hex.EncodeToString(b)
}
