package server

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/FedericoArdesi/mcsync/internal/api"
	"github.com/FedericoArdesi/mcsync/internal/auth"
	"github.com/FedericoArdesi/mcsync/internal/tarutil"
)

// upload riceve il mondo come flusso tar (tarutil.Pack), lo controlla, lo salva
// nell'archivio e chiede il commit.
//
// Il salvataggio avviene anche se chi carica non è l'host: se il commit viene
// poi rifiutato (generazione vecchia, base non più corrente), la copia resta
// nell'archivio come orfano e la risposta ne riporta l'identificativo, così il
// lavoro non si perde (6.2, sezione 7).
//
// Una volta ricevuto tutto il corpo, salvataggio e commit proseguono anche se il
// client si disconnette: farli a metà e poi ripeterli non servirebbe a niente.
// Il client che non ha avuto risposta guarda lo stato (current) per sapere se il
// commit è avvenuto.
func (s *Server) upload(w http.ResponseWriter, r *http.Request, dev auth.Device) {
	gen, ok1 := queryUint(r, api.QueryGeneration)
	parent, ok2 := queryUint(r, api.QueryParent)
	if !ok1 || !ok2 {
		s.failMsg(w, r, http.StatusBadRequest, api.CodeBadRequest, "servono i parametri numerici generation e parent", "")
		return
	}
	maxBody := s.maxBytes + int64(s.maxFiles)*1536 + 1<<20 // contenuto + intestazioni tar + sigillo
	if r.ContentLength > maxBody {
		s.failMsg(w, r, http.StatusRequestEntityTooLarge, api.CodeTooLarge, "il mondo supera i limiti di dimensione del servizio", "")
		return
	}
	release, ok := s.takeSlot(w, r)
	if !ok {
		return
	}
	defer release()

	tmp, err := os.MkdirTemp(s.tmp, "up-")
	if err != nil {
		s.fail(w, r, err, "")
		return
	}
	defer os.RemoveAll(tmp)
	world := filepath.Join(tmp, "world")

	start := time.Now()
	rc := http.NewResponseController(w)
	body := &idleReader{r: http.MaxBytesReader(w, r.Body, maxBody), rc: rc, idle: s.idle}
	err = tarutil.Unpack(body, world, tarutil.Limits{MaxFiles: s.maxFiles, MaxBytes: s.maxBytes})
	// la scadenza va tolta subito: la lettura in background di net/http, che si
	// accorge dei client spariti, parte appena il corpo è finito
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		s.fail(w, r, err, "")
		return
	}
	received := time.Since(start)

	ctx := context.WithoutCancel(r.Context())
	v, res, err := s.svc.Upload(ctx, dev.Player, dev.ID, gen, parent, world)
	if err != nil {
		s.fail(w, r, err, string(res.ID)) // res.ID è vuoto se il salvataggio stesso è fallito
		return
	}
	s.log.Info("upload confermato",
		"id", infoOf(r.Context()).id, "player", dev.Player, "device", dev.ID,
		"version", v.Number, "added_bytes", res.AddedBytes, "total_bytes", res.TotalBytes,
		"files", res.Files, "receive", received.Round(time.Millisecond), "total", time.Since(start).Round(time.Millisecond))
	writeJSON(w, http.StatusOK, api.UploadResponse{
		Version:    versionDTO(v),
		Snapshot:   string(res.ID),
		AddedBytes: res.AddedBytes,
		TotalBytes: res.TotalBytes,
		Files:      res.Files,
	})
}

// download ripristina una versione in una cartella temporanea e la manda come
// flusso tar. La versione scaricata è nell'intestazione X-Mcsync-Version.
//
// Il ripristino finisce (ed è verificato dall'archivio) prima di mandare il
// primo byte, quindi una copia rovinata dà un errore vero e non un flusso a
// metà. Se invece la connessione cade durante l'invio, la risposta viene
// interrotta di proposito e al client manca il sigillo finale: lo rifiuta.
func (s *Server) download(w http.ResponseWriter, r *http.Request, dev auth.Device) {
	version := s.coord.Pointer()
	if r.URL.Query().Has(api.QueryVersion) {
		n, ok := queryUint(r, api.QueryVersion)
		if !ok {
			s.failMsg(w, r, http.StatusBadRequest, api.CodeBadRequest, "version deve essere un numero", "")
			return
		}
		version = n
	} else if version == 0 {
		s.failMsg(w, r, http.StatusNotFound, api.CodeNoVersion, "non esiste ancora nessuna versione ufficiale del mondo", "")
		return
	}
	release, ok := s.takeSlot(w, r)
	if !ok {
		return
	}
	defer release()

	tmp, err := os.MkdirTemp(s.tmp, "down-")
	if err != nil {
		s.fail(w, r, err, "")
		return
	}
	defer os.RemoveAll(tmp)
	world := filepath.Join(tmp, "world")

	start := time.Now()
	if err := s.svc.Download(r.Context(), version, world); err != nil {
		s.fail(w, r, err, "")
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(api.HeaderVersion, strconv.FormatUint(version, 10))
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	if err := tarutil.Pack(&idleWriter{w: w, rc: rc, idle: s.idle}, world, nil); err != nil {
		s.log.Warn("download interrotto", "id", infoOf(r.Context()).id, "device", dev.ID, "version", version, "error", err)
		panic(http.ErrAbortHandler) // il client non deve credere che sia finito
	}
	_ = rc.SetWriteDeadline(time.Time{})
	s.log.Info("download completato", "id", infoOf(r.Context()).id, "player", dev.Player, "device", dev.ID,
		"version", version, "duration", time.Since(start).Round(time.Millisecond))
}

// takeSlot prende uno dei posti per i trasferimenti, senza aspettare: se sono
// tutti occupati risponde 503 con Retry-After.
func (s *Server) takeSlot(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, true
	default:
		w.Header().Set("Retry-After", "30")
		s.failMsg(w, r, http.StatusServiceUnavailable, api.CodeBusy, "il servizio sta già gestendo altri trasferimenti: riprova tra poco", "")
		return nil, false
	}
}

func queryUint(r *http.Request, name string) (uint64, bool) {
	n, err := strconv.ParseUint(r.URL.Query().Get(name), 10, 64)
	return n, err == nil
}

// idleReader e idleWriter spostano in avanti la scadenza della connessione a
// ogni lettura o scrittura: un trasferimento lungo ma vivo non scade mai, uno
// fermo da più di idle sì.
type idleReader struct {
	r    io.Reader
	rc   *http.ResponseController
	idle time.Duration
	last time.Time
}

func (i *idleReader) Read(p []byte) (int, error) {
	if now := time.Now(); now.Sub(i.last) > i.idle/4 {
		_ = i.rc.SetReadDeadline(now.Add(i.idle))
		i.last = now
	}
	return i.r.Read(p)
}

type idleWriter struct {
	w    io.Writer
	rc   *http.ResponseController
	idle time.Duration
	last time.Time
}

func (i *idleWriter) Write(p []byte) (int, error) {
	if now := time.Now(); now.Sub(i.last) > i.idle/4 {
		_ = i.rc.SetWriteDeadline(now.Add(i.idle))
		i.last = now
	}
	return i.w.Write(p)
}
