package tarutil

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mcsync/internal/testutil"
)

// buildTar costruisce un archivio a mano, anche con voci che Pack non farebbe mai.
func buildTar(t *testing.T, hdrs ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		body := ""
		if h.Typeflag == tar.TypeReg {
			if h.Size == 0 {
				h.Size = int64(len("dati"))
			}
			body = strings.Repeat("d", int(h.Size))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return seal(buf.Bytes())
}

// seal aggiunge a un tar il sigillo finale, come fa Pack.
func seal(tarBytes []byte) []byte {
	sum := sha256.Sum256(tarBytes)
	out := append([]byte(nil), tarBytes...)
	out = append(out, sealMagic...)
	return append(out, sum[:]...)
}

func reg(name string) *tar.Header {
	return &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: 4}
}

func TestPackUnpackRoundTrip(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	world := map[string]string{
		"level.dat":        "livello",
		"region/r.0.0.mca": strings.Repeat("R", 5000),
		"players/a.dat":    "giocatore",
		"vuoto.txt":        "",
	}
	testutil.WriteTree(t, src, world)
	if err := os.MkdirAll(filepath.Join(src, "cartella-vuota"), 0o755); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2026, 10, 1, 8, 30, 15, 123456000, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "level.dat"), mt, mt); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Pack(&buf, src, nil); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	if err := Unpack(&buf, dst, Limits{}); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ReadTree(t, dst); !reflect.DeepEqual(got, world) {
		t.Fatalf("contenuto diverso:\n got %v\nwant %v", got, world)
	}
	if st, err := os.Stat(filepath.Join(dst, "cartella-vuota")); err != nil || !st.IsDir() {
		t.Fatalf("la cartella vuota doveva arrivare: %v", err)
	}
	st, _ := os.Stat(filepath.Join(dst, "level.dat"))
	if !st.ModTime().Equal(mt) {
		t.Fatalf("data di modifica = %v, attesa %v", st.ModTime(), mt)
	}
}

func TestPackSkipsExcluded(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	testutil.WriteTree(t, src, map[string]string{
		"level.dat": "x", "logs/a.log": "y", "session.lock": "z",
	})
	skip := func(rel string) bool { return rel == "logs" || rel == "session.lock" }
	var buf bytes.Buffer
	if err := Pack(&buf, src, skip); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "dst")
	if err := Unpack(&buf, dst, Limits{}); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ReadTree(t, dst); !reflect.DeepEqual(got, map[string]string{"level.dat": "x"}) {
		t.Fatalf("contenuto inatteso: %v", got)
	}
}

func TestUnpackRejectsDangerousArchives(t *testing.T) {
	cases := map[string][]*tar.Header{
		"risale con ..":          {reg("../evil.txt")},
		"risale in mezzo":        {reg("a/../../evil.txt")},
		"percorso assoluto":      {reg("/etc/evil.txt")},
		"backslash":              {reg(`a\..\evil.txt`)},
		"lettera di disco":       {reg("C:/evil.txt")},
		"punto iniziale":         {reg("./level.dat")},
		"doppio slash":           {reg("a//b.txt")},
		"nome vuoto":             {reg("")},
		"collegamento simbolico": {{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc/passwd"}},
		"collegamento fisso":     {{Typeflag: tar.TypeLink, Name: "link", Linkname: "level.dat"}},
		"dispositivo":            {{Typeflag: tar.TypeChar, Name: "dev"}},
		"file duplicato":         {reg("level.dat"), reg("level.dat")},
		"cartella ..":            {{Typeflag: tar.TypeDir, Name: "../fuori/", Mode: 0o755}},
	}
	for name, hdrs := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dst := filepath.Join(root, "dst")
			err := Unpack(bytes.NewReader(buildTar(t, hdrs...)), dst, Limits{})
			if !errors.Is(err, ErrBadArchive) {
				t.Fatalf("atteso ErrBadArchive, ottenuto %v", err)
			}
			// niente è uscito dalla cartella di destinazione
			entries, _ := os.ReadDir(root)
			if len(entries) != 1 {
				t.Fatalf("fuori da dst è comparso qualcosa: %v", entries)
			}
		})
	}
}

func TestUnpackRejectsEmptyArchive(t *testing.T) {
	for name, data := range map[string][]byte{
		"nessun byte":   nil,
		"solo coda tar": buildTar(t),
		"solo cartelle": buildTar(t, &tar.Header{Typeflag: tar.TypeDir, Name: "a/", Mode: 0o755}),
	} {
		t.Run(name, func(t *testing.T) {
			err := Unpack(bytes.NewReader(data), filepath.Join(t.TempDir(), "dst"), Limits{})
			if !errors.Is(err, ErrBadArchive) {
				t.Fatalf("atteso ErrBadArchive, ottenuto %v", err)
			}
		})
	}
}

func TestUnpackRejectsTruncatedArchive(t *testing.T) {
	h := reg("grande.bin")
	h.Size = 10000
	full := buildTar(t, h)
	for _, cut := range []int{700, 5000, len(full) - 2000} {
		err := Unpack(bytes.NewReader(full[:cut]), filepath.Join(t.TempDir(), "dst"), Limits{})
		if !errors.Is(err, ErrBadArchive) {
			t.Fatalf("taglio a %d: atteso ErrBadArchive, ottenuto %v", cut, err)
		}
	}
}

func TestUnpackLimits(t *testing.T) {
	data := buildTar(t, reg("a"), reg("b"), reg("c"))
	if err := Unpack(bytes.NewReader(data), filepath.Join(t.TempDir(), "d1"), Limits{MaxFiles: 2}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("troppi file: atteso ErrTooLarge, ottenuto %v", err)
	}
	if err := Unpack(bytes.NewReader(data), filepath.Join(t.TempDir(), "d2"), Limits{MaxBytes: 11}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("troppi byte: atteso ErrTooLarge, ottenuto %v", err)
	}
	if err := Unpack(bytes.NewReader(data), filepath.Join(t.TempDir(), "d3"), Limits{MaxFiles: 3, MaxBytes: 12}); err != nil {
		t.Fatalf("al limite doveva passare: %v", err)
	}
}

func TestUnpackPropagatesReaderError(t *testing.T) {
	boom := errors.New("rete caduta")
	r := io.MultiReader(bytes.NewReader(buildTar(t, reg("a"))[:600]), errReader{boom})
	err := Unpack(r, filepath.Join(t.TempDir(), "dst"), Limits{})
	if !errors.Is(err, boom) || !errors.Is(err, ErrBadArchive) {
		t.Fatalf("atteso l'errore di lettura dentro ErrBadArchive, ottenuto %v", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// Un archivio tagliato esattamente tra una voce e l'altra non dà errori di
// lettura del tar: solo il sigillo finale permette di accorgersene. Deve essere
// rifiutato, perché altrimenti un upload interrotto passerebbe per completo.
func TestUnpackRejectsArchiveCutAtAnyBlock(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	testutil.WriteTree(t, src, map[string]string{
		"level.dat":        "livello",
		"region/r.0.0.mca": strings.Repeat("R", 5000),
		"players/a.dat":    "giocatore",
	})
	var buf bytes.Buffer
	if err := Pack(&buf, src, nil); err != nil {
		t.Fatal(err)
	}
	full := buf.Bytes()
	if err := Unpack(bytes.NewReader(full), filepath.Join(root, "ok"), Limits{}); err != nil {
		t.Fatalf("l'archivio intero doveva passare: %v", err)
	}
	for cut := 0; cut < len(full); cut += 512 {
		dst := filepath.Join(root, fmt.Sprintf("cut%d", cut))
		if err := Unpack(bytes.NewReader(full[:cut]), dst, Limits{}); !errors.Is(err, ErrBadArchive) {
			t.Fatalf("taglio a %d byte su %d: atteso ErrBadArchive, ottenuto %v", cut, len(full), err)
		}
	}
}

func packedWorld(t *testing.T) []byte {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	testutil.WriteTree(t, src, map[string]string{
		"level.dat":        "livello",
		"region/r.0.0.mca": strings.Repeat("R", 5000),
	})
	var buf bytes.Buffer
	if err := Pack(&buf, src, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnpackRejectsDamagedContent(t *testing.T) {
	full := packedWorld(t)
	i := bytes.Index(full, []byte("RRRR")) + 100 // dentro i dati di un file
	damaged := append([]byte(nil), full...)
	damaged[i] = 'X'
	err := Unpack(bytes.NewReader(damaged), filepath.Join(t.TempDir(), "dst"), Limits{})
	if !errors.Is(err, ErrBadArchive) {
		t.Fatalf("un byte cambiato nei dati: atteso ErrBadArchive, ottenuto %v", err)
	}
}

func TestUnpackRejectsDamagedOrCutSeal(t *testing.T) {
	full := packedWorld(t)
	cases := map[string][]byte{
		"sigillo tagliato":      full[:len(full)-10],
		"sigillo senza hash":    full[:len(full)-sha256Size],
		"hash sbagliato":        append(append([]byte(nil), full[:len(full)-1]...), full[len(full)-1]^0xff),
		"scritta sbagliata":     append(append([]byte(nil), full[:len(full)-sealSize]...), append([]byte("xcsync-tar1\n"), full[len(full)-sha256Size:]...)...),
		"dati dopo il sigillo":  append(append([]byte(nil), full...), 'x'),
		"due archivi attaccati": append(append([]byte(nil), full...), full...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			err := Unpack(bytes.NewReader(data), filepath.Join(t.TempDir(), "dst"), Limits{})
			if !errors.Is(err, ErrBadArchive) {
				t.Fatalf("atteso ErrBadArchive, ottenuto %v", err)
			}
		})
	}
}

const sha256Size = sha256.Size
