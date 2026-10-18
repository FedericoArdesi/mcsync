package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mcsync/internal/archive"
	"mcsync/internal/protocol"
	"mcsync/internal/testutil"
)

type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time          { return c.t }
func (c *testClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func setup() (*Service, *protocol.Coordinator, *archive.MemStore, *testClock) {
	clk := &testClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	coord := protocol.NewCoordinator(clk, nil)
	store := archive.NewMemStore()
	return New(coord, store), coord, store, clk
}

func TestUploadThenDownloadRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, coord, _, _ := setup()
	lease, err := coord.Acquire("A", "pcA")
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	src := filepath.Join(root, "world")
	world := map[string]string{
		"level.dat":        "livello",
		"region/r.0.0.mca": "regione-uno",
		"players/a.dat":    "giocatore",
	}
	testutil.WriteTree(t, src, world)

	v, _, err := svc.Upload(ctx, "A", "pcA", lease.Generation, 0, src)
	if err != nil || v.Number != 1 || v.Snapshot == "" {
		t.Fatalf("upload: %+v, %v", v, err)
	}

	dst := filepath.Join(root, "out")
	n, err := svc.DownloadCurrent(ctx, dst)
	if err != nil || n != 1 {
		t.Fatalf("download: n=%d, %v", n, err)
	}
	if got := testutil.ReadTree(t, dst); !reflect.DeepEqual(got, world) {
		t.Fatalf("contenuto diverso:\n got %v\nwant %v", got, world)
	}
}

// Sezione 7: l'host perde la rete a metà upload.
func TestInterruptedUploadLeavesPointerUntouched(t *testing.T) {
	ctx := context.Background()
	svc, coord, store, _ := setup()
	lease, _ := coord.Acquire("A", "pcA")

	src := filepath.Join(t.TempDir(), "world")
	testutil.WriteTree(t, src, map[string]string{
		"level.dat":        "livello",
		"region/r.0.0.mca": "regione",
	})
	boom := errors.New("rete caduta")
	store.BeforeFile = func(rel string) error {
		if rel == "region/r.0.0.mca" {
			return boom
		}
		return nil
	}

	if _, _, err := svc.Upload(ctx, "A", "pcA", lease.Generation, 0, src); !errors.Is(err, boom) {
		t.Fatalf("atteso l'errore simulato, ottenuto %v", err)
	}
	if coord.Pointer() != 0 {
		t.Fatal("il puntatore non doveva muoversi")
	}
	if ids, _ := store.List(ctx); len(ids) != 0 {
		t.Fatalf("nell'archivio non doveva restare niente, ha %v", ids)
	}
	if len(coord.Orphans()) != 0 {
		t.Fatal("un upload interrotto non è un orfano: non c'è mai stato un commit")
	}
}

// Sezione 7: vecchio host che si risveglia in ritardo. Il suo lavoro non si
// perde: resta nell'archivio e il coordinatore lo registra come orfano.
func TestStaleHostUploadIsOrphanedButNotLost(t *testing.T) {
	ctx := context.Background()
	svc, coord, store, clk := setup()
	a, _ := coord.Acquire("A", "pcA")
	clk.Advance(protocol.LeaseTTL + time.Second)
	if _, err := coord.Acquire("B", "pcB"); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(t.TempDir(), "world")
	testutil.WriteTree(t, src, map[string]string{"level.dat": "lavoro-di-A"})

	_, res, err := svc.Upload(ctx, "A", "pcA", a.Generation, 0, src)
	if !errors.Is(err, protocol.ErrStaleGeneration) {
		t.Fatalf("atteso ErrStaleGeneration, ottenuto %v", err)
	}
	if coord.Pointer() != 0 {
		t.Fatal("il puntatore non doveva muoversi")
	}
	orphans := coord.Orphans()
	if len(orphans) != 1 || orphans[0].Request.Snapshot != string(res.ID) {
		t.Fatalf("orfani inattesi: %+v", orphans)
	}
	ids, _ := store.List(ctx)
	if len(ids) != 1 || ids[0] != res.ID {
		t.Fatalf("la copia dell'orfano doveva restare nell'archivio, ha %v", ids)
	}
}

func TestSecondUploadAddsOnlyChangedBytes(t *testing.T) {
	ctx := context.Background()
	svc, coord, _, _ := setup()
	lease, _ := coord.Acquire("A", "pcA")

	src := filepath.Join(t.TempDir(), "world")
	testutil.WriteTree(t, src, map[string]string{
		"level.dat":        "livello",
		"region/r.0.0.mca": strings.Repeat("R", 1000),
	})
	if _, _, err := svc.Upload(ctx, "A", "pcA", lease.Generation, 0, src); err != nil {
		t.Fatal(err)
	}

	testutil.WriteTree(t, src, map[string]string{"level.dat": "livello2"})
	v2, res, err := svc.Upload(ctx, "A", "pcA", lease.Generation, 1, src)
	if err != nil || v2.Number != 2 || v2.Parent != 1 {
		t.Fatalf("secondo upload: %+v, %v", v2, err)
	}
	if res.AddedBytes != int64(len("livello2")) {
		t.Fatalf("AddedBytes = %d, atteso %d (solo il file cambiato)", res.AddedBytes, len("livello2"))
	}
}

func TestDownloadOldVersion(t *testing.T) {
	ctx := context.Background()
	svc, coord, _, _ := setup()
	lease, _ := coord.Acquire("A", "pcA")

	root := t.TempDir()
	src := filepath.Join(root, "world")
	testutil.WriteTree(t, src, map[string]string{"level.dat": "uno"})
	if _, _, err := svc.Upload(ctx, "A", "pcA", lease.Generation, 0, src); err != nil {
		t.Fatal(err)
	}
	testutil.WriteTree(t, src, map[string]string{"level.dat": "due"})
	if _, _, err := svc.Upload(ctx, "A", "pcA", lease.Generation, 1, src); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(root, "vecchia")
	if err := svc.Download(ctx, 1, dst); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"level.dat": "uno"}
	if got := testutil.ReadTree(t, dst); !reflect.DeepEqual(got, want) {
		t.Fatalf("versione 1 diversa:\n got %v\nwant %v", got, want)
	}
}

func TestDownloadUnknownVersion(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := setup()
	root := t.TempDir()
	for _, n := range []uint64{0, 5} {
		if err := svc.Download(ctx, n, filepath.Join(root, "out")); !errors.Is(err, ErrUnknownVersion) {
			t.Fatalf("versione %d: atteso ErrUnknownVersion, ottenuto %v", n, err)
		}
	}
}

func TestCorruptedSnapshotIsNotServed(t *testing.T) {
	ctx := context.Background()
	svc, coord, store, _ := setup()
	lease, _ := coord.Acquire("A", "pcA")

	root := t.TempDir()
	src := filepath.Join(root, "world")
	testutil.WriteTree(t, src, map[string]string{"level.dat": "livello"})
	v, _, err := svc.Upload(ctx, "A", "pcA", lease.Generation, 0, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Corrupt(archive.SnapshotID(v.Snapshot), "level.dat"); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(root, "out")
	if _, err := svc.DownloadCurrent(ctx, dst); !errors.Is(err, archive.ErrCorrupt) {
		t.Fatalf("atteso ErrCorrupt, ottenuto %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("la destinazione non doveva essere creata")
	}
}
