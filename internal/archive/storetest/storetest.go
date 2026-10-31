// Package storetest contiene i test del contratto archive.Store, scritti una
// volta sola e usati da ogni implementazione (archivio finto e restic): se
// passano tutte e due, si comportano allo stesso modo.
package storetest

import (
	"context"
	"errors"
	"math/rand"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/FedericoArdesi/mcsync/internal/archive"
	"github.com/FedericoArdesi/mcsync/internal/testutil"
)

// Run esegue tutti i test del contratto. newStore deve restituire ogni volta
// un archivio nuovo e vuoto.
func Run(t *testing.T, newStore func(t *testing.T) archive.Store) {
	cases := []struct {
		name string
		fn   func(t *testing.T, s archive.Store)
	}{
		{"RoundTripSkipsExcluded", roundTripSkipsExcluded},
		{"DeltaIsSmall", deltaIsSmall},
		{"VersionsAreIndependent", versionsAreIndependent},
		{"ListAndDelete", listAndDelete},
		{"UnknownSnapshot", unknownSnapshot},
		{"GetDestinationExists", getDestinationExists},
		{"CancelledPutLeavesNothing", cancelledPutLeavesNothing},
		{"VerifyHealthy", verifyHealthy},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) { c.fn(t, newStore(t)) })
	}
}

func put(t *testing.T, s archive.Store, src string) archive.PutResult {
	t.Helper()
	res, err := s.Put(context.Background(), src)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return res
}

func get(t *testing.T, s archive.Store, id archive.SnapshotID, dst string) {
	t.Helper()
	if err := s.Get(context.Background(), id, dst); err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
}

func contains(ids []archive.SnapshotID, id archive.SnapshotID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// pseudoRandom restituisce dati incomprimibili ma ripetibili.
func pseudoRandom(n int, seed int64) string {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return string(b)
}

func roundTripSkipsExcluded(t *testing.T, s archive.Store) {
	root := t.TempDir()
	src := filepath.Join(root, "world")
	included := map[string]string{
		"level.dat":          "livello",
		"region/r.0.0.mca":   "regione",
		"players/a.dat":      "giocatore",
		"mods/logs/keep.txt": "tieni",
	}
	all := map[string]string{}
	for k, v := range included {
		all[k] = v
	}
	// file che devono restare fuori dall'archivio
	all["session.lock"] = "lock"
	all["dimensions/x/session.lock"] = "lock2"
	all["logs/latest.log"] = "log"
	all["crash-reports/c.txt"] = "crash"
	all["cache/a"] = "cache"
	testutil.WriteTree(t, src, all)

	res := put(t, s, src)
	if res.Files != len(included) {
		t.Fatalf("file salvati = %d, attesi %d", res.Files, len(included))
	}

	dst := filepath.Join(root, "out")
	get(t, s, res.ID, dst)
	if got := testutil.ReadTree(t, dst); !reflect.DeepEqual(got, included) {
		t.Fatalf("contenuto ripristinato diverso:\n got %v\nwant %v", got, included)
	}
}

func deltaIsSmall(t *testing.T, s archive.Store) {
	src := filepath.Join(t.TempDir(), "world")
	testutil.WriteTree(t, src, map[string]string{
		"region/big.mca": pseudoRandom(300_000, 1),
		"level.dat":      "uno",
	})
	first := put(t, s, src)
	if first.AddedBytes < 100_000 {
		t.Fatalf("la prima copia doveva aggiungere almeno il file grande, ha aggiunto %d byte", first.AddedBytes)
	}

	testutil.WriteTree(t, src, map[string]string{"level.dat": "due-versione-nuova"})
	second := put(t, s, src)
	if second.AddedBytes >= first.AddedBytes/4 {
		t.Fatalf("la seconda copia doveva aggiungere poco: %d byte contro %d della prima", second.AddedBytes, first.AddedBytes)
	}
}

func versionsAreIndependent(t *testing.T, s archive.Store) {
	root := t.TempDir()
	src := filepath.Join(root, "world")
	testutil.WriteTree(t, src, map[string]string{"level.dat": "uno", "region/r.mca": "regione"})
	v1 := put(t, s, src)
	testutil.WriteTree(t, src, map[string]string{"level.dat": "versione-due"})
	v2 := put(t, s, src)

	out1 := filepath.Join(root, "out1")
	out2 := filepath.Join(root, "out2")
	get(t, s, v1.ID, out1)
	get(t, s, v2.ID, out2)

	want1 := map[string]string{"level.dat": "uno", "region/r.mca": "regione"}
	want2 := map[string]string{"level.dat": "versione-due", "region/r.mca": "regione"}
	if got := testutil.ReadTree(t, out1); !reflect.DeepEqual(got, want1) {
		t.Fatalf("versione 1 diversa:\n got %v\nwant %v", got, want1)
	}
	if got := testutil.ReadTree(t, out2); !reflect.DeepEqual(got, want2) {
		t.Fatalf("versione 2 diversa:\n got %v\nwant %v", got, want2)
	}
}

func listAndDelete(t *testing.T, s archive.Store) {
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "world")
	testutil.WriteTree(t, src, map[string]string{"level.dat": "uno"})
	v1 := put(t, s, src)
	testutil.WriteTree(t, src, map[string]string{"level.dat": "versione-due"})
	v2 := put(t, s, src)

	ids, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(ids, v1.ID) || !contains(ids, v2.ID) {
		t.Fatalf("List = %v, doveva contenere %s e %s", ids, v1.ID, v2.ID)
	}

	if err := s.Delete(ctx, v1.ID); err != nil {
		t.Fatal(err)
	}
	ids, err = s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if contains(ids, v1.ID) || !contains(ids, v2.ID) {
		t.Fatalf("dopo Delete List = %v", ids)
	}
	if err := s.Get(ctx, v1.ID, filepath.Join(root, "out")); !errors.Is(err, archive.ErrNotFound) {
		t.Fatalf("Get di una copia cancellata: atteso ErrNotFound, ottenuto %v", err)
	}
}

func unknownSnapshot(t *testing.T, s archive.Store) {
	ctx := context.Background()
	const missing = archive.SnapshotID("non-esiste-questa-snapshot")
	if err := s.Get(ctx, missing, filepath.Join(t.TempDir(), "out")); !errors.Is(err, archive.ErrNotFound) {
		t.Errorf("Get: atteso ErrNotFound, ottenuto %v", err)
	}
	if err := s.Verify(ctx, missing); !errors.Is(err, archive.ErrNotFound) {
		t.Errorf("Verify: atteso ErrNotFound, ottenuto %v", err)
	}
	if err := s.Delete(ctx, missing); !errors.Is(err, archive.ErrNotFound) {
		t.Errorf("Delete: atteso ErrNotFound, ottenuto %v", err)
	}
}

func getDestinationExists(t *testing.T, s archive.Store) {
	root := t.TempDir()
	src := filepath.Join(root, "world")
	testutil.WriteTree(t, src, map[string]string{"a.txt": "a"})
	res := put(t, s, src)

	dst := filepath.Join(root, "out")
	testutil.WriteTree(t, dst, map[string]string{"gia-qui.txt": "x"})
	if err := s.Get(context.Background(), res.ID, dst); !errors.Is(err, archive.ErrDstExists) {
		t.Fatalf("atteso ErrDstExists, ottenuto %v", err)
	}
}

func cancelledPutLeavesNothing(t *testing.T, s archive.Store) {
	src := filepath.Join(t.TempDir(), "world")
	testutil.WriteTree(t, src, map[string]string{"a.txt": "a"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Put(ctx, src); !errors.Is(err, context.Canceled) {
		t.Fatalf("atteso context.Canceled, ottenuto %v", err)
	}
	ids, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("un upload annullato non doveva lasciare copie, ha %v", ids)
	}
}

func verifyHealthy(t *testing.T, s archive.Store) {
	src := filepath.Join(t.TempDir(), "world")
	testutil.WriteTree(t, src, map[string]string{"level.dat": "livello"})
	res := put(t, s, src)
	if err := s.Verify(context.Background(), res.ID); err != nil {
		t.Fatalf("Verify su una copia sana: %v", err)
	}
}
