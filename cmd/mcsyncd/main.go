// Comando mcsyncd: il servizio che gira sul laptop (sezione 4.2 del progetto).
//
//	mcsyncd init   --data DIR                  prepara cartelle, chiave e repository restic
//	mcsyncd serve  --data DIR --listen IP:PORTA avvia il servizio
//	mcsyncd invite --data DIR --addr IP:PORTA GIOCATORE   crea un codice di invito
//	mcsyncd devices --data DIR                 elenca i dispositivi
//	mcsyncd revoke --data DIR ID               revoca un dispositivo
//
// Inviti e revoche si fanno solo sul laptop. Se il servizio è in funzione, i
// comandi parlano con lui tramite un socket Unix nella cartella dati; se è
// fermo, scrivono direttamente il file dei dispositivi.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/FedericoArdesi/mcsync/internal/admin"
	"github.com/FedericoArdesi/mcsync/internal/api"
	"github.com/FedericoArdesi/mcsync/internal/archive"
	"github.com/FedericoArdesi/mcsync/internal/auth"
	"github.com/FedericoArdesi/mcsync/internal/protocol"
	"github.com/FedericoArdesi/mcsync/internal/server"
	"github.com/FedericoArdesi/mcsync/internal/service"
	"github.com/FedericoArdesi/mcsync/internal/statefile"
)

const defaultData = "/srv/mcsync"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// layout è la disposizione della cartella dati (5.5 del progetto).
type layout struct{ root string }

func (l layout) repo() string      { return filepath.Join(l.root, "repo") }
func (l layout) state() string     { return filepath.Join(l.root, "state") }
func (l layout) stateFile() string { return filepath.Join(l.state(), "state.json") }
func (l layout) devices() string   { return filepath.Join(l.state(), "devices.json") }
func (l layout) key() string       { return filepath.Join(l.state(), "repo.key") }
func (l layout) work() string      { return filepath.Join(l.root, "work") }
func (l layout) sock() string      { return filepath.Join(l.state(), "admin.sock") }

func usage(w io.Writer) {
	fmt.Fprint(w, `uso: mcsyncd <comando> [opzioni]

comandi:
  init     prepara la cartella dati: chiave del repository e repository restic
  serve    avvia il servizio (--listen IP:PORTA, l'indirizzo della rete privata)
  invite   crea un codice di invito per un giocatore (--addr IP:PORTA GIOCATORE)
  devices  elenca i dispositivi
  revoke   revoca un dispositivo (ID)

opzioni comuni:
  --data DIR   cartella dati (default `+defaultData+`)

Con "mcsyncd <comando> -h" si vedono le opzioni del comando.
`)
}

func run(ctx context.Context, args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		usage(errw)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(ctx, rest, out, errw)
	case "serve":
		err = cmdServe(ctx, rest, out, errw)
	case "invite":
		err = cmdInvite(rest, out, errw)
	case "devices":
		err = cmdDevices(rest, out, errw)
	case "revoke":
		err = cmdRevoke(rest, out, errw)
	case "help", "-h", "--help":
		usage(out)
		return 0
	default:
		fmt.Fprintf(errw, "comando sconosciuto: %q\n\n", cmd)
		usage(errw)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintln(errw, "errore:", err)
		return 2
	default:
		fmt.Fprintln(errw, "errore:", err)
		return 1
	}
}

var errUsage = errors.New("uso non valido")

func usageErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, a...))
}

func newFlags(name string, errw io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("mcsyncd "+name, flag.ContinueOnError)
	fs.SetOutput(errw)
	data := fs.String("data", envOr("MCSYNC_DATA", defaultData), "cartella dati")
	return fs, data
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	return nil
}

// ---------------------------------------------------------------------------

func cmdInit(ctx context.Context, args []string, out, errw io.Writer) error {
	fs, data := newFlags("init", errw)
	restic := fs.String("restic", "restic", "percorso del programma restic")
	if err := parse(fs, args); err != nil {
		return err
	}
	l := layout{*data}
	for _, d := range []string{l.root, l.state(), l.work()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	created, err := ensureKey(l.key())
	if err != nil {
		return err
	}
	pass, err := readKey(l.key())
	if err != nil {
		return err
	}
	if err := archive.NewResticStore(*restic, l.repo(), pass).EnsureRepo(ctx); err != nil {
		return fmt.Errorf("repository restic: %w", err)
	}
	if created {
		fmt.Fprintf(out, "Creata la chiave del repository: %s\n", l.key())
		fmt.Fprintln(out, "ATTENZIONE: senza questa chiave i mondi salvati non si possono leggere.")
		fmt.Fprintln(out, "Tienine una copia fuori dal laptop (non nel repository, non su un servizio condiviso).")
	}
	fmt.Fprintf(out, "Pronto: %s\n", l.root)
	return nil
}

// ensureKey crea la chiave del repository se manca. Non la sovrascrive mai.
func ensureKey(path string) (created bool, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(base64.RawURLEncoding.EncodeToString(b) + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return false, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return false, err
	}
	return true, f.Close()
}

func readKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("manca la chiave del repository (%s): esegui prima \"mcsyncd init\"", path)
	}
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("la chiave del repository (%s) è vuota", path)
	}
	return k, nil
}

// ---------------------------------------------------------------------------

func cmdServe(ctx context.Context, args []string, out, errw io.Writer) error {
	fs, data := newFlags("serve", errw)
	listen := fs.String("listen", "", "indirizzo IP:PORTA su cui ascoltare (quello della rete privata)")
	restic := fs.String("restic", "restic", "percorso del programma restic")
	reserveGB := fs.Float64("reserve-gb", 20, "spazio libero da lasciare sul laptop, in GB (rifiuta i commit oltre)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *listen == "" {
		return usageErr("manca --listen (l'indirizzo della rete privata, per esempio 100.x.y.z:8765)")
	}
	if err := server.CheckListenAddr(*listen); err != nil {
		return usageErr("%v", err)
	}
	l := layout{*data}
	pass, err := readKey(l.key())
	if err != nil {
		return err
	}
	store := archive.NewResticStore(*restic, l.repo(), pass)
	if _, err := store.List(ctx); err != nil {
		return fmt.Errorf("il repository non risponde (hai eseguito \"mcsyncd init\"?): %w", err)
	}
	log := slog.New(slog.NewTextHandler(errw, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var free func() int64
	if _, err := freeSpace(l.root); err != nil {
		log.Warn("controllo dello spazio libero non disponibile: i commit non verificano lo spazio", "error", err)
	} else {
		free = freeBytesFunc(l.repo(), uint64(*reserveGB*(1<<30)))
	}

	srv, adm, err := build(l, store, protocol.SystemClock{}, free, log)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	adminDone := make(chan error, 1)
	if runtime.GOOS == "windows" {
		log.Warn("amministrazione a servizio acceso non disponibile su Windows: inviti e revoche solo a servizio fermo")
		adminDone <- nil
	} else {
		go func() { adminDone <- admin.Serve(ctx, l.sock(), admin.Handler(adm)) }()
	}
	err = srv.ListenAndServe(ctx, *listen)
	cancel()
	if aerr := <-adminDone; aerr != nil && err == nil {
		err = fmt.Errorf("amministrazione locale: %w", aerr)
	}
	return err
}

// freeBytesFunc restituisce lo spazio che il laptop può ancora dare ai commit:
// quello libero meno la riserva. Se non riesce a leggerlo non blocca niente.
func freeBytesFunc(path string, reserve uint64) func() int64 {
	return func() int64 {
		f, err := freeSpace(path)
		if err != nil {
			return math.MaxInt64
		}
		if f <= reserve {
			return 0
		}
		return int64(f - reserve)
	}
}

// build mette insieme coordinatore (con stato su disco), dispositivi, servizio
// e server. Restituisce anche il gestore dell'amministrazione locale.
func build(l layout, store archive.Store, clock protocol.Clock, free func() int64, log *slog.Logger) (*server.Server, *auth.Store, error) {
	for _, d := range []string{l.state(), l.work()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, nil, err
		}
	}
	coord, err := statefile.Open(l.stateFile(), clock, free)
	if err != nil {
		return nil, nil, err
	}
	devs, err := auth.Open(l.devices(), clock)
	if err != nil {
		return nil, nil, err
	}
	srv, err := server.New(server.Config{
		Coordinator: coord,
		Service:     service.New(coord, store),
		Auth:        devs,
		Clock:       clock,
		WorkDir:     l.work(),
		Logger:      log,
	})
	if err != nil {
		return nil, nil, err
	}
	return srv, devs, nil
}

// ---------------------------------------------------------------------------
// Amministrazione: a servizio acceso tramite socket, a servizio fermo sul file.

type adminOps interface {
	Invite(player string) (string, error)
	Devices() ([]auth.Device, error)
	Revoke(id string) error
}

type directOps struct{ s *auth.Store }

func (d directOps) Invite(p string) (string, error) { return d.s.CreateInvite(p) }
func (d directOps) Devices() ([]auth.Device, error) { return d.s.Devices(), nil }
func (d directOps) Revoke(id string) error          { return d.s.Revoke(id) }

func openAdmin(l layout) (ops adminOps, viaService bool, err error) {
	if runtime.GOOS != "windows" && admin.Running(l.sock()) {
		return admin.NewClient(l.sock()), true, nil
	}
	s, err := auth.Open(l.devices(), protocol.SystemClock{})
	if err != nil {
		return nil, false, err
	}
	return directOps{s}, false, nil
}

func cmdInvite(args []string, out, errw io.Writer) error {
	fs, data := newFlags("invite", errw)
	addr := fs.String("addr", "", "indirizzo IP:PORTA del servizio, come lo vedranno i giocatori")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErr("serve il nome del giocatore: mcsyncd invite --addr IP:PORTA GIOCATORE")
	}
	if err := server.CheckListenAddr(*addr); err != nil {
		return usageErr("--addr: %v", err)
	}
	player := fs.Arg(0)
	if !auth.ValidPlayer(player) {
		return usageErr("%v (3-16 caratteri tra lettere, cifre e trattino basso)", auth.ErrBadPlayer)
	}
	ops, _, err := openAdmin(layout{*data})
	if err != nil {
		return err
	}
	secret, err := ops.Invite(player)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Codice di invito per %s (vale una volta sola, scade tra %d ore):\n\n%s\n\n",
		player, int(auth.InviteTTL.Hours()), api.EncodeInviteCode(*addr, secret))
	fmt.Fprintln(out, "Mandalo solo a lui: chi ha il codice può iscrivere un dispositivo a suo nome.")
	return nil
}

func cmdDevices(args []string, out, errw io.Writer) error {
	fs, data := newFlags("devices", errw)
	if err := parse(fs, args); err != nil {
		return err
	}
	ops, _, err := openAdmin(layout{*data})
	if err != nil {
		return err
	}
	devs, err := ops.Devices()
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Fprintln(out, "Nessun dispositivo iscritto.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tGIOCATORE\tETICHETTA\tCREATO\tSTATO")
	for _, d := range devs {
		state := "attivo"
		if d.Revoked {
			state = "revocato"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.ID, d.Player, d.Label, d.CreatedAt.Local().Format("2006-01-02 15:04"), state)
	}
	return tw.Flush()
}

func cmdRevoke(args []string, out, errw io.Writer) error {
	fs, data := newFlags("revoke", errw)
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErr("serve l'ID del dispositivo (lo trovi con mcsyncd devices)")
	}
	ops, _, err := openAdmin(layout{*data})
	if err != nil {
		return err
	}
	if err := ops.Revoke(fs.Arg(0)); err != nil {
		return err
	}
	fmt.Fprintf(out, "Dispositivo %s revocato: il suo token non funziona più.\n", fs.Arg(0))
	return nil
}
