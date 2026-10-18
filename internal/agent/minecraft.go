package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Righe della console del server usate come segnali (verificate nel
// prototipo 1b, server 26.3): "Done (" a fine avvio, "Saved the game" a fine
// di "save-all flush". Niente RCON: i comandi vanno nello stdin.
const (
	logReady = "Done ("
	logSaved = "Saved the game"
)

// MinecraftGame è il server dedicato di Minecraft come processo figlio
// dell'agente (4.3 del progetto). Implementa Game.
//
// Il mondo da sincronizzare è la cartella indicata a Start: il server la
// riceve con --universe (la cartella che la contiene) e --world (il suo nome),
// così non serve toccare server.properties. Gli altri file del server
// (server.jar, eula.txt, server.properties, whitelist) restano nella cartella
// del server e non fanno parte del mondo sincronizzato.
//
// Il processo non è legato al contesto di Start: un Ctrl+C non lo uccide, lo
// ferma Stop con il comando "stop" (e solo se non risponde lo termina).
type MinecraftGame struct {
	Java      string   // programma java (default "java")
	Jar       string   // percorso di server.jar
	ServerDir string   // cartella di lavoro del server (default: quella di Jar)
	JVMArgs   []string // per esempio -Xmx2G
	Log       *slog.Logger

	StartTimeout time.Duration // attesa di "Done (", default 5 minuti
	SaveTimeout  time.Duration // attesa di "Saved the game", default 60 secondi

	done chan struct{}

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	waiters []*waiter
	tail    []string
	exitErr error
}

type waiter struct {
	sub string
	ch  chan struct{}
}

// NewMinecraftGame crea il gioco con il programma java e il server.jar dati.
func NewMinecraftGame(java, jar string) *MinecraftGame {
	return &MinecraftGame{Java: java, Jar: jar, done: make(chan struct{})}
}

var _ Game = (*MinecraftGame)(nil)

func (g *MinecraftGame) Done() <-chan struct{} { return g.done }

func (g *MinecraftGame) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (g *MinecraftGame) exited() bool {
	select {
	case <-g.done:
		return true
	default:
		return false
	}
}

// expect registra l'attesa di una riga che contenga sub. Va chiamata prima di
// mandare il comando che la provoca.
func (g *MinecraftGame) expect(sub string) *waiter {
	w := &waiter{sub: sub, ch: make(chan struct{})}
	g.mu.Lock()
	g.waiters = append(g.waiters, w)
	g.mu.Unlock()
	return w
}

func (g *MinecraftGame) forget(w *waiter) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, x := range g.waiters {
		if x == w {
			g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
			return
		}
	}
}

func (g *MinecraftGame) onLine(line string) {
	g.log().Debug("server", "riga", line)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tail = append(g.tail, line)
	if len(g.tail) > 20 {
		g.tail = g.tail[len(g.tail)-20:]
	}
	kept := g.waiters[:0]
	for _, w := range g.waiters {
		if strings.Contains(line, w.sub) {
			close(w.ch)
		} else {
			kept = append(kept, w)
		}
	}
	g.waiters = kept
}

func (g *MinecraftGame) tailString() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.tail) == 0 {
		return "(nessun output)"
	}
	return strings.Join(g.tail, " | ")
}

func (g *MinecraftGame) send(command string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stdin == nil {
		return errors.New("il server non è avviato")
	}
	_, err := fmt.Fprintln(g.stdin, command)
	return err
}

func (g *MinecraftGame) kill() {
	g.mu.Lock()
	cmd := g.cmd
	g.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		<-g.done
	}
}

// Start avvia il server sul mondo in world e aspetta la riga "Done (".
// Restituisce un errore se il server si ferma prima (il caso tipico è
// eula.txt con eula=false) o se non è pronto entro StartTimeout.
func (g *MinecraftGame) Start(ctx context.Context, world string) error {
	if g.Jar == "" {
		return errors.New("manca il percorso di server.jar")
	}
	if g.Java == "" {
		g.Java = "java"
	}
	if g.StartTimeout <= 0 {
		g.StartTimeout = 5 * time.Minute
	}
	if g.SaveTimeout <= 0 {
		g.SaveTimeout = time.Minute
	}
	jar, err := filepath.Abs(g.Jar)
	if err != nil {
		return err
	}
	if st, err := os.Stat(jar); err != nil || st.IsDir() {
		return fmt.Errorf("server.jar non trovato: %s", jar)
	}
	world, err = filepath.Abs(world)
	if err != nil {
		return err
	}
	universe, name := filepath.Dir(world), filepath.Base(world)
	if err := os.MkdirAll(universe, 0o755); err != nil {
		return err
	}
	dir := g.ServerDir
	if dir == "" {
		dir = filepath.Dir(jar)
	}

	args := append([]string{}, g.JVMArgs...)
	args = append(args, "-jar", jar, "--nogui", "--universe", universe, "--world", name)
	cmd := exec.Command(g.Java, args...)
	cmd.Dir = dir
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	ready := g.expect(logReady)
	if err := cmd.Start(); err != nil {
		g.forget(ready)
		_ = pw.Close()
		return fmt.Errorf("avvio di %q non riuscito (Java installato? percorso giusto?): %w", g.Java, err)
	}
	g.mu.Lock()
	g.cmd, g.stdin = cmd, stdin
	g.mu.Unlock()
	g.log().Info("server Minecraft avviato, aspetto che sia pronto", "java", g.Java, "jar", jar, "mondo", world)

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			g.onLine(strings.TrimRight(sc.Text(), "\r"))
		}
		_, _ = io.Copy(io.Discard, pr) // una riga enorme non deve bloccare il server
	}()
	go func() {
		err := cmd.Wait()
		_ = pw.Close()
		<-scanDone
		g.mu.Lock()
		g.exitErr = err
		g.mu.Unlock()
		close(g.done)
	}()

	timer := time.NewTimer(g.StartTimeout)
	defer timer.Stop()
	select {
	case <-ready.ch:
		g.log().Info("server Minecraft pronto")
		return nil
	case <-g.done:
		g.forget(ready)
		msg := fmt.Sprintf("il server Minecraft si è fermato prima di essere pronto; ultime righe: %s", g.tailString())
		if strings.Contains(strings.ToLower(g.tailString()), "eula") {
			msg += " — apri eula.txt nella cartella del server e imposta eula=true"
		}
		return errors.New(msg)
	case <-ctx.Done():
		g.forget(ready)
		g.kill()
		return ctx.Err()
	case <-timer.C:
		g.forget(ready)
		g.kill()
		return fmt.Errorf("il server Minecraft non è pronto dopo %s; ultime righe: %s", g.StartTimeout, g.tailString())
	}
}

// Freeze fa scrivere tutto su disco e sospende i salvataggi automatici
// ("save-off", "save-all flush", attesa di "Saved the game"). Se il server non
// conferma entro SaveTimeout (per esempio perché è in pausa senza giocatori,
// caso ancora da verificare) riattiva i salvataggi e restituisce un errore:
// l'upload di quel giro viene saltato, non fatto su un mondo non scritto.
func (g *MinecraftGame) Freeze(ctx context.Context) (func(), error) {
	if g.exited() {
		return nil, errors.New("il server Minecraft è fermo")
	}
	if err := g.send("save-off"); err != nil {
		return nil, fmt.Errorf("save-off: %w", err)
	}
	saved := g.expect(logSaved)
	defer g.forget(saved)
	fail := func(err error) (func(), error) {
		_ = g.send("save-on")
		return nil, err
	}
	if err := g.send("save-all flush"); err != nil {
		return fail(fmt.Errorf("save-all flush: %w", err))
	}
	t := time.NewTimer(g.SaveTimeout)
	defer t.Stop()
	select {
	case <-saved.ch:
	case <-g.done:
		return nil, errors.New("il server Minecraft si è fermato durante il salvataggio")
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-t.C:
		return fail(fmt.Errorf("il server non ha confermato il salvataggio entro %s", g.SaveTimeout))
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if err := g.send("save-on"); err != nil && !g.exited() {
				g.log().Warn("save-on non inviato", "errore", err)
			}
		})
	}, nil
}

// Stop manda "stop" e aspetta che il server esca; se non esce prima della
// scadenza di ctx lo termina con la forza e lo dice nell'errore.
func (g *MinecraftGame) Stop(ctx context.Context) error {
	g.mu.Lock()
	cmd := g.cmd
	g.mu.Unlock()
	if cmd == nil || g.exited() {
		return nil
	}
	_ = g.send("stop")
	select {
	case <-g.done:
		return nil
	case <-ctx.Done():
		g.kill()
		return errors.New("il server Minecraft non si è fermato in tempo: terminato con la forza, il mondo potrebbe non essere coerente")
	}
}
