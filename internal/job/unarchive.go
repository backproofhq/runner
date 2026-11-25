package job

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/backproofhq/runner/internal/archive"
)

// maxLoggedEntries caps how many file names from an archive one log line lists.
const maxLoggedEntries = 10

// workFile is a file the unpack loop reads: the backup itself, or a file the runner wrote.
type workFile struct {
	file *os.File
	path string
	size int64
	// owned files were written by the runner, so they can be removed as soon as they're read.
	owned bool
}

type entryKind int

const (
	entryOther entryKind = iota
	entryDump
	entryArchive
)

// contents is what an archive holds, by the names of its entries.
type contents struct {
	dumps     []string
	archives  []string
	protected int
}

// pick reports whether to unpack the archive's one dump, or else its one archive.
func (c contents) pick() (bool, error) {
	switch {
	case len(c.dumps) == 1:
		return true, nil
	case len(c.dumps) > 1:
		return false, ambiguousArchive(fmt.Sprintf("The archive holds %d Postgres dumps, so the runner can't tell which one to test. Keep one dump per archive.", len(c.dumps)), c.dumps)
	case len(c.archives) == 1:
		return false, nil
	case len(c.archives) > 1:
		return false, ambiguousArchive(fmt.Sprintf("The archive holds %d other archives but no Postgres dump, so the runner can't tell which one to test. Keep one dump per archive.", len(c.archives)), c.archives)
	case c.protected > 0:
		failure := RunnerError("protected_archive", "The zip archive is password-protected, which the runner can't open. Encrypt your backups with age or GPG instead.", nil)
		failure.Stage = StageDecrypt
		return false, failure
	default:
		return false, Fail("no_dump_in_archive", "The archive holds no Postgres dump. Point the source at your database backups, or check the job that creates them.")
	}
}

// ambiguousArchive names the entries only in the runner's log, because they don't leave the runner.
func ambiguousArchive(reason string, names []string) error {
	listed := names[:min(len(names), maxLoggedEntries)]
	return RunnerError("ambiguous_archive", reason, fmt.Errorf("found %s", strings.Join(listed, ", ")))
}

func damagedBundle(error) error {
	return Fail("damaged_archive", "The archive is damaged or incomplete, so it can't be unpacked. Check the job that creates your backups.")
}

type unpacker struct {
	ctx       context.Context //nolint:containedctx // One unpack runs within one call to prepareDump.
	directory string
	options   Options
	layers    *layerChain
	files     int
}

// openArchive writes the archive's one Postgres dump, from any folder, to the work directory. No
// other entry is written, because archives can hold unrelated files such as keys.
func (u *unpacker) openArchive(format archive.Format, source io.Reader, current workFile, onFile bool, depth int) (workFile, error) {
	defer u.discard(current)
	if depth >= maxLayers {
		return workFile{}, tooManyLayers()
	}
	if format == archive.FormatTar {
		return u.openTar(source, depth)
	}
	if !onFile {
		// A zip lists its entries at the end, so it has to be on disk to be read.
		written, err := u.write(source, -1)
		if err != nil {
			return workFile{}, err
		}
		defer u.discard(written)
		current = written
	}
	return u.openZip(current, depth)
}

func (u *unpacker) openZip(current workFile, depth int) (workFile, error) {
	reader, err := zip.NewReader(current.file, current.size)
	if err != nil {
		return workFile{}, damagedBundle(err)
	}

	var found contents
	var dumps, archives []*zip.File
	for _, entry := range reader.File {
		if err := u.ctx.Err(); err != nil {
			return workFile{}, fmt.Errorf("read archive: %w", err)
		}
		if !entry.Mode().IsRegular() {
			continue
		}
		if entry.Flags&0x1 != 0 {
			found.protected++
			continue
		}
		switch u.probeZip(entry, depth) {
		case entryDump:
			dumps = append(dumps, entry)
			found.dumps = append(found.dumps, entry.Name)
		case entryArchive:
			archives = append(archives, entry)
			found.archives = append(found.archives, entry.Name)
		case entryOther:
		}
	}

	dump, err := found.pick()
	if err != nil {
		return workFile{}, err
	}
	chosen := archives
	if dump {
		chosen = dumps
	}
	entry, err := chosen[0].Open()
	if err != nil {
		return workFile{}, damagedBundle(err)
	}
	defer func() { _ = entry.Close() }()
	return u.write(&contextReader{ctx: u.ctx, reader: entry}, int64(min(chosen[0].UncompressedSize64, math.MaxInt64)))
}

func (u *unpacker) probeZip(entry *zip.File, depth int) entryKind {
	reader, err := entry.Open()
	if err != nil {
		return entryOther
	}
	defer func() { _ = reader.Close() }()
	return u.probe(reader, depth+1)
}

// openTar reads the archive once, because it may be streamed from a compressed or encrypted file.
// It writes the first dump it finds, or the first archive while it has found no dump.
func (u *unpacker) openTar(source io.Reader, depth int) (workFile, error) {
	reader := tar.NewReader(source)
	var found contents
	var dump, nested workFile
	defer func() {
		u.discard(dump)
		u.discard(nested)
	}()

	for len(found.dumps) < 2 {
		if err := u.ctx.Err(); err != nil {
			return workFile{}, fmt.Errorf("read archive: %w", err)
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			// Reading past the end marker reaches the end of any compression, which checks for damage.
			if _, err := io.Copy(io.Discard, source); err != nil {
				return workFile{}, u.layers.failure(err, damagedBundle)
			}
			break
		}
		if err != nil {
			return workFile{}, u.layers.failure(err, damagedBundle)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}

		var head bytes.Buffer
		kind := u.probe(io.TeeReader(reader, &head), depth+1)
		switch kind {
		case entryDump:
			found.dumps = append(found.dumps, header.Name)
			if len(found.dumps) > 1 {
				continue
			}
		case entryArchive:
			found.archives = append(found.archives, header.Name)
			if len(found.dumps) > 0 || len(found.archives) > 1 {
				continue
			}
		case entryOther:
			continue
		}

		written, err := u.write(io.MultiReader(&head, reader), header.Size)
		if err != nil {
			return workFile{}, err
		}
		if kind == entryDump {
			u.discard(nested)
			dump, nested = written, workFile{}
		} else {
			nested = written
		}
	}

	useDump, err := found.pick()
	if err != nil {
		return workFile{}, err
	}
	var chosen workFile
	if useDump {
		chosen, dump = dump, workFile{}
	} else {
		chosen, nested = nested, workFile{}
	}
	return chosen, nil
}

// probe looks at the start of an entry, through any compression or encryption, to tell whether it
// holds a Postgres dump or another archive.
func (u *unpacker) probe(source io.Reader, depth int) entryKind {
	var closers []io.Closer
	defer func() {
		for _, closer := range closers {
			_ = closer.Close()
		}
	}()

	for ; depth <= maxLayers; depth++ {
		buffered := bufio.NewReaderSize(source, archive.SniffSize)
		sample, err := buffered.Peek(archive.SniffSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return entryOther
		}

		switch format := archive.SniffBytes(sample); format {
		case archive.FormatCustom:
			return entryDump
		case archive.FormatPlain:
			if archive.HasPlainDumpHeader(sample) {
				return entryDump
			}
			return entryOther
		case archive.FormatZip, archive.FormatTar:
			return entryArchive
		case archive.FormatGzip, archive.FormatZstd:
			decompressed, err := archive.Decompress(format, buffered)
			if err != nil {
				return entryOther
			}
			closers = append(closers, decompressed)
			source = decompressed
		case archive.FormatAge, archive.FormatPGP:
			decrypted, err := openEncrypted(format, buffered, u.options)
			if err != nil {
				// Unpacking it reports the missing or wrong key.
				return entryDump
			}
			source = decrypted
		case archive.FormatEmpty, archive.FormatUnknown:
			return entryOther
		}
	}
	return entryOther
}

// write copies an entry to a new work file. size is the entry's size, or -1 when it isn't known.
func (u *unpacker) write(source io.Reader, size int64) (workFile, error) {
	if err := os.MkdirAll(u.directory, 0o700); err != nil {
		return workFile{}, WorkFileFailure(err)
	}
	if size >= 0 {
		free, err := freeSpace(u.directory)
		if err != nil {
			return workFile{}, err
		}
		if free < float64(size) {
			return workFile{}, RunnerError("disk_too_small", fmt.Sprintf(
				"The work directory has %s free, but the dump inside the archive needs %s. Free some space or use a larger volume.",
				readableSize(free), readableSize(float64(size)),
			), nil)
		}
	}

	u.files++
	path := filepath.Join(u.directory, fmt.Sprintf("unpacked-%d", u.files))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec // The path is built from the runner's own work directory.
	if err != nil {
		return workFile{}, WorkFileFailure(err)
	}
	u.layers.closers = append(u.layers.closers, file)
	written := workFile{file: file, path: path, owned: true}

	output := &trackedWriter{writer: file}
	written.size, err = io.Copy(output, source)
	if err != nil {
		u.discard(written)
		if output.err != nil {
			return workFile{}, WorkFileFailure(output.err)
		}
		return workFile{}, u.layers.failure(err, damagedBundle)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		u.discard(written)
		return workFile{}, WorkFileFailure(err)
	}
	return written, nil
}

// discard removes a work file early, so an archive and its unpacked entry don't both take up space.
func (u *unpacker) discard(work workFile) {
	if !work.owned {
		return
	}
	_ = work.file.Close()
	_ = os.Remove(work.path)
}
