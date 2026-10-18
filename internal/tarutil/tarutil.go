// Package tarutil mette una cartella in un flusso tar e la rimette giù, per
// far viaggiare il mondo tra client e servizio.
//
// Il flusso è un normale tar seguito da un sigillo di 44 byte: la scritta
// "mcsync-tar1" e lo SHA-256 di tutto il tar. Il sigillo serve perché il formato
// tar non sa riconoscere un flusso interrotto esattamente tra una voce e
// l'altra: senza, un upload tagliato a metà passerebbe per completo. Con il
// sigillo, un flusso troncato, rovinato o con dati in più viene rifiutato
// (6.2, punto 1: "blocchi arrivati per intero e hash corrispondenti").
//
// Il lato che riceve (Unpack) non si fida del flusso: accetta solo file e
// cartelle normali, rifiuta i nomi che uscirebbero dalla cartella di
// destinazione, e rispetta dei limiti di dimensione.
package tarutil

import (
	"archive/tar"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var (
	// ErrBadArchive: il flusso non è un archivio accettabile (nome pericoloso,
	// tipo di voce non ammesso, archivio troncato o vuoto).
	ErrBadArchive = errors.New("archivio non valido")
	// ErrTooLarge: troppi file o troppi byte rispetto ai limiti.
	ErrTooLarge = errors.New("archivio troppo grande")
)

// Limits sono i limiti di Unpack. Zero vuol dire nessun limite.
type Limits struct {
	MaxFiles int
	MaxBytes int64
}

// Pack scrive in w il contenuto di dir: file normali (con data di modifica) e
// cartelle. Le voci che non sono né file né cartelle (collegamenti, ecc.) si
// saltano. skip, se non è nil, dice quali percorsi relativi lasciare fuori.
func Pack(w io.Writer, dir string, skip func(rel string) bool) error {
	sum := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(w, sum))
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir, Name: rel + "/", Mode: 0o755,
				ModTime: info.ModTime(), Format: tar.FormatPAX,
			})
		case d.Type().IsRegular():
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: rel, Mode: 0o644, Size: info.Size(),
				ModTime: info.ModTime(), Format: tar.FormatPAX,
			}); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			// CopyN: se il file cresce mentre lo leggiamo, si prende solo la
			// dimensione dichiarata; se si accorcia, tar segnala l'errore.
			if _, err := io.CopyN(tw, f, info.Size()); err != nil {
				return fmt.Errorf("lettura di %s: %w", rel, err)
			}
			return nil
		default:
			return nil
		}
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	_, err = w.Write(append([]byte(sealMagic), sum.Sum(nil)...))
	return err
}

// Unpack legge un archivio da r e lo scrive in dst (creata se manca, e da
// usare vuota). Un archivio senza nessun file è rifiutato: un upload vuoto non
// deve mai poter sostituire un mondo. Se restituisce un errore, dst può
// contenere file a metà: chi chiama la cancella.
func Unpack(r io.Reader, dst string, lim Limits) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	cr := &hashingReader{r: r, h: sha256.New()}
	tr := tar.NewReader(cr)
	seen := map[string]bool{}
	var files int
	var total int64

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrBadArchive, err)
		}
		name, err := cleanName(hdr.Name)
		if err != nil {
			return err
		}
		full := filepath.Join(dst, filepath.FromSlash(name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(full, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if hdr.Size < 0 {
				return fmt.Errorf("%w: dimensione negativa per %q", ErrBadArchive, name)
			}
			if seen[name] {
				return fmt.Errorf("%w: %q compare due volte", ErrBadArchive, name)
			}
			seen[name] = true
			files++
			total += hdr.Size
			if (lim.MaxFiles > 0 && files > lim.MaxFiles) || (lim.MaxBytes > 0 && total > lim.MaxBytes) {
				return ErrTooLarge
			}
			if err := writeFile(tr, full, hdr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: tipo di voce non ammesso per %q", ErrBadArchive, name)
		}
	}
	if files == 0 {
		return fmt.Errorf("%w: nessun file", ErrBadArchive)
	}
	return cr.checkSeal()
}

const (
	sealMagic = "mcsync-tar1\n"
	sealSize  = len(sealMagic) + sha256.Size
)

// hashingReader calcola lo SHA-256 di quello che il lettore di tar consuma.
// tar.Reader legge a blocchi esatti e si ferma alla coda dell'archivio, quindi
// dopo l'ultima voce il sigillo è ancora tutto da leggere in r.
type hashingReader struct {
	r io.Reader
	h hash.Hash
}

func (c *hashingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.h.Write(p[:n])
	return n, err
}

// checkSeal legge il sigillo dopo la coda del tar e controlla che lo SHA-256
// corrisponda e che dopo non ci sia altro.
func (c *hashingReader) checkSeal() error {
	want := c.h.Sum(nil)
	var seal [sealSize]byte
	if _, err := io.ReadFull(c.r, seal[:]); err != nil {
		return fmt.Errorf("%w: archivio interrotto, manca il sigillo finale (%w)", ErrBadArchive, err)
	}
	if string(seal[:len(sealMagic)]) != sealMagic || !hmac.Equal(seal[len(sealMagic):], want) {
		return fmt.Errorf("%w: il sigillo finale non corrisponde al contenuto", ErrBadArchive)
	}
	var extra [1]byte
	if _, err := io.ReadFull(c.r, extra[:]); err != io.EOF {
		return fmt.Errorf("%w: dati dopo il sigillo finale", ErrBadArchive)
	}
	return nil
}

func writeFile(tr *tar.Reader, full string, hdr *tar.Header) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	// O_EXCL: non si scrive mai sopra a qualcosa che esiste già
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, tr)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadArchive, err)
	}
	if n != hdr.Size {
		return fmt.Errorf("%w: %q più corto del dichiarato", ErrBadArchive, hdr.Name)
	}
	if !hdr.ModTime.IsZero() {
		if err := os.Chtimes(full, hdr.ModTime, hdr.ModTime); err != nil {
			return err
		}
	}
	return nil
}

// cleanName accetta solo percorsi relativi, con "/" come separatore, già in
// forma pulita e senza ".." in nessun punto.
func cleanName(name string) (string, error) {
	bad := func() (string, error) {
		return "", fmt.Errorf("%w: nome non ammesso %q", ErrBadArchive, name)
	}
	n := strings.TrimSuffix(name, "/")
	if n == "" || n == "." || strings.ContainsAny(n, "\\:\x00") || path.IsAbs(n) || path.Clean(n) != n {
		return bad()
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return bad()
		}
	}
	return n, nil
}
