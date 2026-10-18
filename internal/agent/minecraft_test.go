package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcsync/internal/tarutil"
)

// TestFakeMinecraftProcess non è un test: è il finto server Minecraft. Il
// programma di test viene rilanciato con FAKE_MC=1 e si comporta come la
// console del server vero (stdin e stdout), così MinecraftGame si prova senza
// Java. FAKE_MC_MODE: "" normale, "noeula" esce subito, "nosave" non conferma
// il salvataggio, "hang" ignora stop, "crash" esce da solo dopo l'avvio.
func TestFakeMinecraftProcess(t *testing.T) {
	if os.Getenv("FAKE_MC") != "1" {
		return
	}
	var universe, world string
	for i, a := range os.Args {
		switch a {
		case "--universe":
			universe = os.Args[i+1]
		case "--world":
			world = os.Args[i+1]
		}
	}
	mode := os.Getenv("FAKE_MC_MODE")
	say := func(s string) { fmt.Printf("[12:00:00] [Server thread/INFO]: %s\n", s) }
	if mode == "noeula" {
		fmt.Println("[main/ERROR]: You need to agree to the EULA in order to run the server. Go to eula.txt for more info.")
		os.Exit(1)
	}
	_ = os.MkdirAll(filepath.Join(universe, world), 0o755)
	_ = os.WriteFile(filepath.Join(universe, world, "level.dat"), []byte("mondo vero\n"), 0o644)
	logf, _ := os.OpenFile(filepath.Join(universe, "cmds.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	time.Sleep(30 * time.Millisecond)
	say(`Done (0.03s)! For help, type "help"`)
	if mode == "crash" {
		time.Sleep(50 * time.Millisecond)
		os.Exit(1)
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		cmd := strings.TrimSpace(sc.Text())
		fmt.Fprintln(logf, cmd)
		switch cmd {
		case "save-off":
			say("Automatic saving is now disabled")
		case "save-all flush":
			if mode != "nosave" {
				say("Saving the game (this may take a moment!)")
				_ = os.WriteFile(filepath.Join(universe, world, "ticks.txt"), []byte(time.Now().String()), 0o644)
				say("Saved the game")
			}
		case "save-on":
			say("Automatic saving is now enabled")
		case "stop":
			if mode == "hang" {
				continue
			}
			say("Stopping the server")
			logf.Close()
			os.Exit(0)
		}
	}
	os.Exit(0)
}

func newFakeMC(t *testing.T, mode string) (*MinecraftGame, string) {
	t.Helper()
	t.Setenv("FAKE_MC", "1")
	t.Setenv("FAKE_MC_MODE", mode)
	root := t.TempDir()
	jar := filepath.Join(root, "server.jar")
	if err := os.WriteFile(jar, []byte("finto"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := NewMinecraftGame(os.Args[0], jar)
	g.JVMArgs = []string{"-test.run=^TestFakeMinecraftProcess$", "--"}
	g.StartTimeout = 10 * time.Second
	g.SaveTimeout = 600 * time.Millisecond
	return g, filepath.Join(root, "mondo")
}

func cmdsLog(t *testing.T, world string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(world), "cmds.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "save-all flush", "save-all-flush"))
}

func TestMinecraftStartFreezeStop(t *testing.T) {
	g, world := newFakeMC(t, "")
	if err := g.Start(context.Background(), world); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(world, "level.dat")); string(b) != "mondo vero\n" {
		t.Errorf("level.dat = %q: il server non ha ricevuto --universe/--world giusti", b)
	}
	thaw, err := g.Freeze(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	thaw()
	thaw() // idempotente
	if err := g.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-g.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done non si è chiuso dopo lo stop")
	}
	got := strings.Join(cmdsLog(t, world), ",")
	if got != "save-off,save-all-flush,save-on,stop" {
		t.Errorf("comandi mandati = %s", got)
	}
}

func TestMinecraftExitsBeforeReadyExplainsEULA(t *testing.T) {
	g, world := newFakeMC(t, "noeula")
	err := g.Start(context.Background(), world)
	if err == nil || !strings.Contains(err.Error(), "eula=true") || !strings.Contains(err.Error(), "EULA") {
		t.Fatalf("errore = %v", err)
	}
	if err := g.Stop(context.Background()); err != nil {
		t.Errorf("Stop dopo un avvio fallito: %v", err)
	}
}

func TestMinecraftMissingJarAndJava(t *testing.T) {
	g := NewMinecraftGame("java", filepath.Join(t.TempDir(), "non-esiste.jar"))
	if err := g.Start(context.Background(), filepath.Join(t.TempDir(), "m")); err == nil || !strings.Contains(err.Error(), "server.jar non trovato") {
		t.Errorf("errore = %v", err)
	}
	root := t.TempDir()
	jar := filepath.Join(root, "server.jar")
	_ = os.WriteFile(jar, []byte("x"), 0o644)
	g = NewMinecraftGame(filepath.Join(root, "java-che-non-esiste"), jar)
	if err := g.Start(context.Background(), filepath.Join(root, "m")); err == nil || !strings.Contains(err.Error(), "Java installato") {
		t.Errorf("errore = %v", err)
	}
	if err := g.Stop(context.Background()); err != nil {
		t.Errorf("Stop senza processo: %v", err)
	}
}

func TestMinecraftFreezeTimeoutReenablesSaving(t *testing.T) {
	g, world := newFakeMC(t, "nosave")
	if err := g.Start(context.Background(), world); err != nil {
		t.Fatal(err)
	}
	defer g.Stop(context.Background())
	thaw, err := g.Freeze(context.Background())
	if err == nil || thaw != nil {
		t.Fatalf("Freeze senza conferma doveva fallire: thaw=%v err=%v", thaw != nil, err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := strings.Join(cmdsLog(t, world), ","); got != "save-off,save-all-flush,save-on" {
		t.Errorf("comandi = %s: dopo il fallimento i salvataggi automatici devono tornare accesi", got)
	}
}

func TestMinecraftStopKillsHungServer(t *testing.T) {
	g, world := newFakeMC(t, "hang")
	if err := g.Start(context.Background(), world); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := g.Stop(ctx)
	if err == nil || !strings.Contains(err.Error(), "con la forza") {
		t.Fatalf("errore = %v", err)
	}
	select {
	case <-g.Done():
	default:
		t.Error("il processo è ancora vivo")
	}
}

func TestMinecraftCrashClosesDoneAndFreezeFails(t *testing.T) {
	g, world := newFakeMC(t, "crash")
	if err := g.Start(context.Background(), world); err != nil {
		t.Fatal(err)
	}
	select {
	case <-g.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done non si è chiuso dopo il crash")
	}
	if _, err := g.Freeze(context.Background()); err == nil {
		t.Error("Freeze su un server morto doveva fallire")
	}
}

func TestAgentRunsWithMinecraftGame(t *testing.T) {
	g, world := newFakeMC(t, "")
	f := newFake()
	o := fastOpts(t)
	o.World = world
	o.Game = g
	o.UploadEvery = 150 * time.Millisecond
	if err := runFor(t, f, o, 1200*time.Millisecond); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.pointer == 0 || !f.released {
		t.Fatalf("pointer=%d released=%v", f.pointer, f.released)
	}
	// la versione salvata contiene il mondo scritto dal server finto
	chk := filepath.Join(t.TempDir(), "w")
	if err := tarutil.Unpack(bytes.NewReader(f.versions[f.pointer]), chk, tarutil.Limits{MaxFiles: 100, MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(chk, "level.dat")); string(b) != "mondo vero\n" {
		t.Errorf("level.dat nella versione salvata = %q", b)
	}
	cmds := cmdsLog(t, world)
	if len(cmds) < 4 || cmds[0] != "save-off" || cmds[len(cmds)-1] != "stop" {
		t.Errorf("comandi = %v", cmds)
	}
	for i, c := range cmds { // ogni save-off è seguito da save-all-flush e da save-on
		if c == "save-off" && (i+2 >= len(cmds) || cmds[i+1] != "save-all-flush" || cmds[i+2] != "save-on") {
			t.Errorf("sequenza rotta in posizione %d: %v", i, cmds)
		}
	}
}

func TestGameThatDoesNotStartIsReportedAndLeaseReleased(t *testing.T) {
	f := newFake()
	f.seed(t, map[string]string{"level.dat": "ufficiale"})
	o := fastOpts(t)
	o.Game = NewMinecraftGame("java", filepath.Join(t.TempDir(), "manca.jar"))
	err := runFor(t, f, o, 2*time.Second)
	if !errors.Is(err, ErrGameNotStarted) {
		t.Fatalf("errore = %v, atteso ErrGameNotStarted", err)
	}
	if !f.released {
		t.Error("lease non rilasciata dopo un gioco che non parte")
	}
	if f.uploads != 0 {
		t.Errorf("uploads = %d: senza gioco non c'è niente da caricare", f.uploads)
	}
	st, _ := ReadState(o.workDir())
	if st.Dirty || st.BaseVersion != 1 {
		t.Errorf("stato = %+v, attesa base 1 e non dirty", st)
	}
}

func TestGameThatDoesNotStartOnFirstSessionLeavesNoFalseAlarm(t *testing.T) {
	f := newFake() // nessuna versione ufficiale, mondo locale vuoto
	o := fastOpts(t)
	o.Game = NewMinecraftGame("java", filepath.Join(t.TempDir(), "manca.jar"))
	err := runFor(t, f, o, 2*time.Second)
	if !errors.Is(err, ErrGameNotStarted) || errors.Is(err, ErrNotSaved) {
		t.Fatalf("errore = %v", err)
	}
	if !f.released {
		t.Error("lease non rilasciata")
	}
}
