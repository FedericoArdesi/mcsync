package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/FedericoArdesi/mcsync/internal/testutil"
)

func TestExcluded(t *testing.T) {
	cases := map[string]bool{
		"session.lock":              true,
		"dimensions/x/session.lock": true,
		"logs":                      true,
		"logs/latest.log":           true,
		"crash-reports/c.txt":       true,
		"cache/a":                   true,
		"level.dat":                 false,
		"region/logs.mca":           false,
		"players/data/a.dat":        false,
	}
	for rel, want := range cases {
		if got := Excluded(rel); got != want {
			t.Errorf("Excluded(%q) = %v, atteso %v", rel, got, want)
		}
	}
}

func TestPutGetRoundTripSkipsExcluded(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "world")
	testutil.WriteTree(t, src, map[string]string{
		"level.dat":        "livello",
		"region/r.0.0.mca": "regione",
		"players/a.dat":    "giocatore",
		"session.lock":     "lock",
		"logs/latest.log":  "log",
	})

	s := NewMemStore()
	res, err := s.Put(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 {
		t.Fatalf("file salvati = %d, attesi 3 (esclusi session.lock e logs)", res.Files)
	}

	dst := filepath.Join(root, "out")
	if err := s.Get(ctx, res.ID, dst); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"level.dat":        "livello",
		"region/r.0.0.mca": "regione",
		"players/a.dat":    "giocatore",
	}
	if got := testutil.ReadTree(t, dst); !reflect.DeepEqual(got, want) {
		t.Fatalf("contenuto ripristinato diverso:\n got %v\nwant %v", got, want)
	}
	if _, err := os.Stat(dst + ".partial"); !os.IsNotExist(err) {
		t.Fatal("la cartella provvisoria doveva sparire")
	}
}

func TestPutAddsOnlyChangedBytes(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "world")
	testutil.WriteTree(t, src, map[string]string{"a.txt": "aaaa", "b.txt": "bbbb"})

	s := NewMemStore()
	first, err := s.Put(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if first.AddedBytes != 8 {
		t.Fatalf("prima copia: AddedBytes = %d, atteso 8", first.AddedBytes)
	}

	testutil.WriteTree(t, src, map[string]string{"b.txt": "cc"})
	second, err := s.Put(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if second.AddedBytes != 2 {
		t.Fatalf("seconda copia: AddedBytes = %d, atteso 2 (solo il file cambiato)", second.AddedBytes)
	}
	if second.TotalBytes != 6 {
		t.Fatalf("seconda copia: TotalBytes = %d, atteso 6", second.TotalBytes)
	}
}

func TestPutInterruptedLeavesNothing(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "world")
	testutil.WriteTree(t, src, map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"})

	boom := errors.New("rete caduta")
	s := NewMemStore()
	s.BeforeFile = func(rel string) error {
		if rel == "b.txt" {
			return boom
		}
		return nil
	}
	if _, err := s.Put(ctx, src); !errors.Is(err, boom) {
		t.Fatalf("atteso l'errore simulato, ottenuto %v", err)
	}
	ids, _ := s.List(ctx)
	if len(ids) != 0 {
		t.Fatalf("dopo un upload interrotto l'archivio doveva restare vuoto, ha %v", ids)
	}
}

func TestPutCancelledContext(t *testing.T) {
	src := filepath.Join(t.TempDir(), "world")
	testutil.WriteTree(t, src, map[string]string{"a.txt": "a"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewMemStore()
	if _, err := s.Put(ctx, src); !errors.Is(err, context.Canceled) {
		t.Fatalf("atteso context.Canceled, ottenuto %v", err)
	}
	ids, _ := s.List(context.Background())
	if len(ids) != 0 {
		t.Fatal("nessuna copia doveva essere salvata")
	}
}

func TestVerifyAndGetDetectCorruption(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "world")
	testutil.WriteTree(t, src, map[string]string{"level.dat": "livello"})

	s := NewMemStore()
	res, err := s.Put(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(ctx, res.ID); err != nil {
		t.Fatalf("copia sana: %v", err)
	}
	if err := s.Corrupt(res.ID, "level.dat"); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(ctx, res.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify: atteso ErrCorrupt, ottenuto %v", err)
	}
	dst := filepath.Join(root, "out")
	if err := s.Get(ctx, res.ID, dst); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get: atteso ErrCorrupt, ottenuto %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("con un blocco corrotto la destinazione non doveva essere creata")
	}
}

func TestGetDestinationExists(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "world")
	testutil.WriteTree(t, src, map[string]string{"a.txt": "a"})

	s := NewMemStore()
	res, _ := s.Put(ctx, src)
	dst := filepath.Join(root, "out")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Get(ctx, res.ID, dst); !errors.Is(err, ErrDstExists) {
		t.Fatalf("atteso ErrDstExists, ottenuto %v", err)
	}
}

func TestUnknownSnapshot(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	if err := s.Verify(ctx, "snap-9999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Verify: atteso ErrNotFound, ottenuto %v", err)
	}
	if err := s.Delete(ctx, "snap-9999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete: atteso ErrNotFound, ottenuto %v", err)
	}
}
