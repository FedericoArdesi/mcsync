package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FakeGame finge di essere il server Minecraft: a intervalli regolari riscrive
// qualche file del mondo (come farebbero i file di regione), e rispetta
// Freeze come farebbe save-off. Serve a provare tutto il giro dell'agente
// (lease, download, heartbeat, upload, chiusura) senza avviare Minecraft.
type FakeGame struct {
	// Every è ogni quanto scrive (default 2 secondi).
	Every time.Duration
	// FileBytes è la dimensione di ogni file scritto (default 64 KiB).
	FileBytes int

	mu   sync.Mutex // preso da chi scrive e da Freeze
	stop chan struct{}
	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// NewFakeGame crea un gioco finto.
func NewFakeGame() *FakeGame {
	return &FakeGame{stop: make(chan struct{}), done: make(chan struct{})}
}

func (g *FakeGame) Done() <-chan struct{} { return g.done }

func (g *FakeGame) Start(_ context.Context, world string) error {
	if g.Every <= 0 {
		g.Every = 2 * time.Second
	}
	if g.FileBytes <= 0 {
		g.FileBytes = 64 << 10
	}
	if err := os.MkdirAll(filepath.Join(world, "region"), 0o755); err != nil {
		return err
	}
	// come Minecraft: crea i file di base se mancano
	lv := filepath.Join(world, "level.dat")
	if _, err := os.Stat(lv); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(lv, []byte("mondo finto\n"), 0o644); err != nil {
			return err
		}
	}
	g.wg.Add(1)
	go g.loop(world)
	return nil
}

func (g *FakeGame) loop(world string) {
	defer g.wg.Done()
	t := time.NewTicker(g.Every)
	defer t.Stop()
	for n := 0; ; n++ {
		select {
		case <-g.stop:
			return
		case <-t.C:
		}
		g.mu.Lock()
		_ = g.write(world, n)
		g.mu.Unlock()
	}
}

// write riscrive un file di regione (a rotazione tra tre) e un contatore.
func (g *FakeGame) write(world string, n int) error {
	data := make([]byte, g.FileBytes)
	if _, err := rand.Read(data); err != nil {
		return err
	}
	name := filepath.Join(world, "region", fmt.Sprintf("r.%d.0.mca", n%3))
	if err := os.WriteFile(name, data, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(world, "ticks.txt"), []byte(fmt.Sprintf("%d\n", n+1)), 0o644)
}

// Freeze ferma le scritture finché non si chiama thaw.
func (g *FakeGame) Freeze(context.Context) (func(), error) {
	select {
	case <-g.done:
		return nil, errors.New("il gioco è fermo")
	default:
	}
	g.mu.Lock()
	var once sync.Once
	return func() { once.Do(g.mu.Unlock) }, nil
}

func (g *FakeGame) Stop(context.Context) error {
	g.once.Do(func() { close(g.stop) })
	g.wg.Wait()
	select {
	case <-g.done:
	default:
		close(g.done)
	}
	return nil
}
