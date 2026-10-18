package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sotto", "file.json")
	if err := Write(path, []byte("uno"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("due"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "due" {
		t.Fatalf("contenuto = %q, atteso %q", got, "due")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("nella cartella doveva restare solo il file, ci sono %d voci", len(entries))
	}
}

func TestStaleTempFileIsOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.json")
	if err := os.WriteFile(path+".tmp", []byte("pezzo a meta"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("intero"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "intero" {
		t.Fatalf("contenuto = %q", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("il .tmp doveva sparire")
	}
}

func TestWriteFailsWhenParentIsAFile(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocco")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(filepath.Join(blocker, "file.json"), []byte("x"), 0o600); err == nil {
		t.Fatal("atteso un errore")
	}
}
