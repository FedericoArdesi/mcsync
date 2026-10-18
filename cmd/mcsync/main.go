// Comando mcsync: l'agente da terminale sul PC del giocatore (4.3 del
// progetto), nella versione minima con un server Minecraft finto.
//
//	mcsync enroll CODICE [--label NOME]   si iscrive al servizio con il codice di invito
//	mcsync status                         mostra chi è host e l'ultima versione
//	mcsync run --world DIR [--server-jar JAR --java JAVA --jvm-args "-Xmx2G"]
//	                                      prende la lease, scarica il mondo, gioca, salva
//	                                      (senza --server-jar: gioco finto)
//
// Il token del dispositivo sta in un file di configurazione solo per il
// proprietario (default: cartella di configurazione dell'utente).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mcsync/internal/agent"
	"mcsync/internal/api"
	"mcsync/internal/atomicfile"
	"mcsync/internal/client"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// config è quello che l'iscrizione lascia sul PC.
type config struct {
	Addr     string `json:"addr"`
	Token    string `json:"token"`
	DeviceID string `json:"device_id"`
	Player   string `json:"player"`
}

var errUsage = errors.New("uso non valido")

func usage(w io.Writer) {
	fmt.Fprint(w, `uso: mcsync <comando> [opzioni]

comandi:
  enroll CODICE   iscrive questo PC con il codice di invito (--label NOME)
  status          mostra lo stato del servizio
  run             avvia una sessione (--world CARTELLA)

opzioni comuni:
  --config FILE   file di configurazione (default: cartella di configurazione dell'utente)

Con "mcsync <comando> -h" si vedono le opzioni del comando.
`)
}

func run(ctx context.Context, args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		usage(errw)
		return 2
	}
	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "enroll":
		err = cmdEnroll(ctx, rest, out, errw)
	case "status":
		err = cmdStatus(ctx, rest, out, errw)
	case "run":
		err = cmdRun(ctx, rest, out, errw)
	case "help", "-h", "--help":
		usage(out)
		return 0
	default:
		fmt.Fprintf(errw, "comando sconosciuto: %q\n\n", cmd)
		usage(errw)
		return 2
	}
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintln(errw, "errore:", err)
		return 2
	default:
		fmt.Fprintln(errw, "errore:", err)
		return 1
	}
}

func newFlags(name string, errw io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("mcsync "+name, flag.ContinueOnError)
	fs.SetOutput(errw)
	cfg := fs.String("config", os.Getenv("MCSYNC_CONFIG"), "file di configurazione")
	return fs, cfg
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

// parseMixed come parse, ma accetta le opzioni anche dopo gli argomenti
// (il pacchetto flag si fermerebbe al primo argomento senza trattino):
// "enroll CODICE --label X" e "enroll --label X CODICE" valgono uguale.
// Restituisce gli argomenti senza trattino, nell'ordine.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := parse(fs, args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func configPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cartella di configurazione non trovata (usa --config): %w", err)
	}
	return filepath.Join(d, "mcsync", "config.json"), nil
}

func loadConfig(path string) (config, error) {
	var c config
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, fmt.Errorf("questo PC non è iscritto (manca %s): lancia prima \"mcsync enroll CODICE\"", path)
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil || c.Addr == "" || c.Token == "" {
		return c, fmt.Errorf("configurazione rovinata (%s): rifai l'iscrizione con un nuovo invito", path)
	}
	return c, nil
}

func cmdEnroll(ctx context.Context, args []string, out, errw io.Writer) error {
	fs, cfgFlag := newFlags("enroll", errw)
	host, _ := os.Hostname()
	label := fs.String("label", host, "nome di questo dispositivo")
	pos, err := parseMixed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: serve il codice di invito (mcs1....)", errUsage)
	}
	path, err := configPath(*cfgFlag)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("questo PC è già iscritto (%s): per iscriverlo di nuovo cancella quel file", path)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	addr, resp, err := client.Enroll(cctx, pos[0], *label)
	if err != nil {
		if client.IsCode(err, api.CodeBadInvite) {
			return errors.New("invito non valido, già usato o scaduto: chiedi un invito nuovo")
		}
		return fmt.Errorf("iscrizione: %w", err)
	}
	b, err := json.MarshalIndent(config{Addr: addr, Token: resp.Token, DeviceID: resp.DeviceID, Player: resp.Player}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := atomicfile.Write(path, b, 0o600); err != nil {
		return fmt.Errorf("il token non è stato salvato (l'invito è già usato: serve un invito nuovo): %w", err)
	}
	fmt.Fprintf(out, "Iscritto come %s (dispositivo %s).\nConfigurazione salvata in %s\n", resp.Player, resp.DeviceID, path)
	return nil
}

func connect(cfgFlag string) (*client.Client, config, error) {
	path, err := configPath(cfgFlag)
	if err != nil {
		return nil, config{}, err
	}
	c, err := loadConfig(path)
	if err != nil {
		return nil, c, err
	}
	return client.New(c.Addr, c.Token), c, nil
}

func cmdStatus(ctx context.Context, args []string, out, errw io.Writer) error {
	fs, cfgFlag := newFlags("status", errw)
	if err := parse(fs, args); err != nil {
		return err
	}
	cl, _, err := connect(*cfgFlag)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	st, err := cl.Status(cctx)
	if err != nil {
		return fmt.Errorf("il laptop non risponde: %w", err)
	}
	fmt.Fprintf(out, "Tu: %s (dispositivo %s)\n", st.You.Player, st.You.Device)
	if st.Pointer == 0 {
		fmt.Fprintln(out, "Versione ufficiale: nessuna (il mondo non è ancora stato salvato)")
	} else if st.Current != nil {
		fmt.Fprintf(out, "Versione ufficiale: %d, di %s, %s\n", st.Pointer, st.Current.Author, st.Current.CreatedAt.Local().Format("02/01/2006 15:04"))
	} else {
		fmt.Fprintf(out, "Versione ufficiale: %d\n", st.Pointer)
	}
	switch {
	case st.Lease != nil:
		fmt.Fprintf(out, "Host: %s (la lease scade tra %d s se non rinnovata)\n", st.Lease.Player, st.Lease.ExpiresInSeconds)
	case st.Reservation != nil:
		fmt.Fprintf(out, "Host: nessuno, lease riservata a %s per altri %d s\n", st.Reservation.For, st.Reservation.ExpiresInSeconds)
	default:
		fmt.Fprintln(out, "Host: nessuno (puoi iniziare tu)")
	}
	return nil
}

func cmdRun(ctx context.Context, args []string, out, errw io.Writer) error {
	fs, cfgFlag := newFlags("run", errw)
	world := fs.String("world", "", "cartella del mondo su questo PC (obbligatoria)")
	workDir := fs.String("work", "", "cartella di lavoro (default: MONDO.mcsync)")
	uploadEvery := fs.Duration("upload-every", 10*time.Minute, "ogni quanto salvare sul laptop")
	hbEvery := fs.Duration("heartbeat-every", time.Minute, "ogni quanto rinnovare la lease")
	fakeEvery := fs.Duration("fake-every", 2*time.Second, "ogni quanto il gioco finto scrive")
	jar := fs.String("server-jar", "", "percorso di server.jar: se c'è si usa il server Minecraft vero, altrimenti il gioco finto")
	java := fs.String("java", "java", "programma java del server (es. C:\\...\\java.exe)")
	jvmArgs := fs.String("jvm-args", "", "opzioni per la JVM, separate da spazi (es. \"-Xmx2G\")")
	verbose := fs.Bool("v", false, "log più dettagliato")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *world == "" {
		return fmt.Errorf("%w: serve --world", errUsage)
	}
	cl, c, err := connect(*cfgFlag)
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(errw, &slog.HandlerOptions{Level: level}))
	var game agent.Game
	if *jar != "" {
		mg := agent.NewMinecraftGame(*java, *jar)
		mg.JVMArgs = strings.Fields(*jvmArgs)
		mg.Log = log
		game = mg
	} else {
		fg := agent.NewFakeGame()
		fg.Every = *fakeEvery
		game = fg
		fmt.Fprintln(out, "Gioco finto (nessun --server-jar): non è Minecraft.")
	}
	fmt.Fprintf(out, "Sessione di %s. Ctrl+C per chiudere: il mondo viene salvato prima di rilasciare la lease.\n", c.Player)

	err = agent.Run(ctx, cl, agent.Options{
		World: *world, WorkDir: *workDir, Game: game, Log: log,
		UploadEvery: *uploadEvery, HeartbeatEvery: *hbEvery,
	})
	var held *agent.LeaseHeldError
	switch {
	case err == nil:
		fmt.Fprintln(out, "Fatto: mondo salvato e lease rilasciata.")
	case errors.As(err, &held):
		return fmt.Errorf("non puoi giocare adesso: %w", err)
	case errors.Is(err, agent.ErrGameNotStarted):
		return fmt.Errorf("%w\nIl mondo non è stato toccato e la lease è stata rilasciata", err)
	case errors.Is(err, agent.ErrNotSaved):
		return fmt.Errorf("ATTENZIONE, mondo non salvato sul laptop: %s\nLa copia sul PC è al sicuro (cartella %s). Non cancellarla e riprova a lanciare mcsync run appena il laptop torna raggiungibile", strings.TrimSpace(err.Error()), *world)
	default:
		return err
	}
	return nil
}
