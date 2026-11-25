package job

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/backproofhq/runner/internal/archive"
	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/crypto"
)

const (
	jobsDirectory = "jobs"
	// Real backups have an archive, encryption and compression at most, so more layers mean a damaged file.
	maxLayers = 4
)

// Options configures one job.
type Options struct {
	Checks   Checks
	WorkDir  string
	SourceID string
	Secrets  config.SourceSecrets
	// DiskMargin is how many times the backup's size must be free before downloading it.
	DiskMargin float64
	// PostgresHint is the major version to use when the backup doesn't say which one made it.
	PostgresHint int
	// OnStage is told when each stage starts.
	OnStage func(Stage)
	// RunningJobs lists the other jobs in progress, whose containers and files must be kept.
	RunningJobs func() []string
}

func jobDirectory(workDir, jobID string) string {
	return filepath.Join(workDir, jobsDirectory, jobID)
}

// prepareDump decrypts, decompresses and unarchives the backup until a Postgres dump remains. Unpacked files
// go into the job's work directory, which RunLocal removes when the job ends.
func prepareDump(ctx context.Context, path, jobID string, options Options, backup *Backup) (Dump, error) {
	unreadable := func(err error) error {
		return RunnerError("file_unreadable", "The backup file could not be read. Check the path and its permissions.", err)
	}

	file, err := os.Open(path) //nolint:gosec // The person running the command chooses this file.
	if err != nil {
		return Dump{}, unreadable(err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return Dump{}, unreadable(err)
	}
	if !info.Mode().IsRegular() {
		return Dump{}, unreadable(fmt.Errorf("%s is not a regular file", filepath.Base(path)))
	}
	backup.SizeBytes = info.Size()
	backup.Time = info.ModTime().UTC()

	layers := &layerChain{}
	defer layers.close()
	unpacker := &unpacker{ctx: ctx, directory: jobDirectory(options.WorkDir, jobID), options: options, layers: layers}
	current := workFile{file: file, path: path, size: info.Size()}
	// onFile is true while source reads current directly, so a dump can be used where it is.
	onFile := true
	source := layers.add(&contextReader{ctx: ctx, reader: file}, unreadable)

	for depth := 0; depth <= maxLayers; depth++ {
		buffered := bufio.NewReaderSize(source, archive.SniffSize)
		sample, err := buffered.Peek(archive.SniffSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return Dump{}, layers.failure(err, damagedArchive)
		}

		format := archive.SniffBytes(sample)
		backup.Format = format
		switch format {
		case archive.FormatCustom, archive.FormatPlain:
			if onFile {
				return finishDump(current.file, current.size, Dump{Path: current.path, Format: format})
			}
			defer unpacker.discard(current)
			return writeUnpacked(buffered, layers, options.WorkDir, jobID, format)

		case archive.FormatGzip, archive.FormatZstd:
			backup.Compression = string(format)
			decompressed, err := archive.Decompress(format, buffered)
			if err != nil {
				return Dump{}, layers.failure(err, damagedArchive)
			}
			layers.closers = append(layers.closers, decompressed)
			source = layers.add(decompressed, damagedArchive)
			onFile = false

		case archive.FormatAge, archive.FormatPGP:
			backup.Encryption = string(format)
			decrypted, err := openEncrypted(format, buffered, options)
			if err != nil {
				return Dump{}, layers.failure(err, func(err error) error { return decryptFailure(err, format, options.SourceID) })
			}
			source = layers.add(decrypted, damagedEncryption)
			onFile = false

		case archive.FormatZip, archive.FormatTar:
			entry, err := unpacker.openArchive(format, buffered, current, onFile, depth)
			if err != nil {
				return Dump{}, err
			}
			current = entry
			onFile = true
			source = layers.add(&contextReader{ctx: ctx, reader: entry.file}, WorkFileFailure)

		case archive.FormatEmpty:
			return Dump{}, Fail("empty_backup", "The backup file is empty. Check the job that creates your backups.")

		default:
			return Dump{}, Fail("unknown_format", "The file isn't a Postgres plain SQL or custom-format dump, or an archive that holds one. Check the job that creates your backups.")
		}
	}
	return Dump{}, tooManyLayers()
}

func tooManyLayers() error {
	return Fail("unknown_format", "The file has too many layers of archives, compression or encryption. Check the job that creates your backups.")
}

func openEncrypted(format archive.Format, encrypted io.Reader, options Options) (io.Reader, error) {
	if format == archive.FormatAge {
		reader, err := crypto.OpenAge(encrypted, options.Secrets.AgeIdentity)
		if err != nil {
			return nil, fmt.Errorf("open age backup: %w", err)
		}
		return reader, nil
	}
	reader, err := crypto.OpenGPG(encrypted, options.Secrets.GPGKey, options.Secrets.GPGPassphrase)
	if err != nil {
		return nil, fmt.Errorf("open gpg backup: %w", err)
	}
	return reader, nil
}

func decryptFailure(err error, format archive.Format, sourceID string) error {
	name, variable := "GPG", config.EnvSourceGPGKey(sourceID)
	if format == archive.FormatAge {
		name, variable = "age", config.EnvSourceAgeIdentity(sourceID)
	}

	var failure *Failure
	switch {
	case errors.Is(err, crypto.ErrNoKey):
		failure = RunnerError("missing_key", fmt.Sprintf("This backup is encrypted with %s, but no key is set. Set %s and try again.", name, variable), nil)
	case errors.Is(err, crypto.ErrInvalidKey):
		failure = RunnerError("invalid_key", fmt.Sprintf("The %s key in %s can't be used. Check the key and its passphrase, then try again.", name, variable), nil)
	case errors.Is(err, crypto.ErrWrongKey):
		failure = Fail("wrong_key", fmt.Sprintf("The configured %s key doesn't open this backup. Check which key your backup job encrypts with.", name))
	default:
		return damagedEncryption(err)
	}
	failure.Stage = StageDecrypt
	return failure
}

func damagedEncryption(error) error {
	failure := Fail("damaged_encryption", "The encrypted backup is damaged or incomplete, so it can't be decrypted. Check the job that creates your backups.")
	failure.Stage = StageDecrypt
	return failure
}

func damagedArchive(error) error {
	return Fail("damaged_archive", "The compressed backup is damaged or incomplete, so it can't be decompressed. Check the job that creates your backups.")
}

func writeUnpacked(source io.Reader, layers *layerChain, workDir, jobID string, format archive.Format) (Dump, error) {
	directory := jobDirectory(workDir, jobID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return Dump{}, WorkFileFailure(err)
	}
	path := filepath.Join(directory, "backup")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec // The path is built from the runner's own work directory.
	if err != nil {
		return Dump{}, WorkFileFailure(err)
	}
	defer func() { _ = file.Close() }()

	output := &trackedWriter{writer: file}
	if _, err := io.Copy(output, source); err != nil {
		if output.err != nil {
			return Dump{}, WorkFileFailure(output.err)
		}
		return Dump{}, layers.failure(err, damagedArchive)
	}
	if err := file.Sync(); err != nil {
		return Dump{}, WorkFileFailure(err)
	}

	info, err := file.Stat()
	if err != nil {
		return Dump{}, WorkFileFailure(err)
	}
	return finishDump(file, info.Size(), Dump{Path: path, Format: format})
}

// WorkFileFailure explains why the runner couldn't write to its work directory.
func WorkFileFailure(err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return RunnerError("disk_full", "The work directory ran out of disk space while unpacking the backup. Free some space and run it again.", err)
	}
	return RunnerError("work_dir_unavailable", "The runner can't write to its work directory. Set "+config.EnvWorkDir+" to a writable directory and try again.", err)
}

func finishDump(file io.ReaderAt, size int64, dump Dump) (Dump, error) {
	if dump.Format != archive.FormatPlain {
		return dump, nil
	}
	truncated, err := archive.PlainDumpTruncated(file, size)
	if err != nil {
		return Dump{}, RunnerError("file_unreadable", "The backup file could not be read. Check the path and its permissions.", err)
	}
	if truncated {
		return Dump{}, Fail("truncated_backup", "The backup file ends before pg_dump finished writing it. Check the job that creates your backups.")
	}
	return dump, nil
}

// removeStaleWorkFiles deletes files a killed run left behind, which can include a decrypted dump.
func removeStaleWorkFiles(ctx context.Context, logger *slog.Logger, workDir string, keepJobIDs ...string) {
	if workDir == "" {
		return
	}
	directory := filepath.Join(workDir, jobsDirectory)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		logger.WarnContext(ctx, "read work directory", slog.Any("error", err))
		return
	}
	for _, entry := range entries {
		if slices.Contains(keepJobIDs, entry.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(directory, entry.Name())); err != nil {
			logger.WarnContext(ctx, "remove leftover work files", slog.Any("error", err))
		}
	}
}

// layerChain remembers which layer failed first, because outer layers only see a broken stream.
type layerChain struct {
	layers  []*trackedReader
	closers []io.Closer
}

func (c *layerChain) add(reader io.Reader, classify func(error) error) io.Reader {
	layer := &trackedReader{reader: reader, classify: classify}
	c.layers = append(c.layers, layer)
	return layer
}

func (c *layerChain) failure(err error, fallback func(error) error) error {
	for _, layer := range c.layers {
		if layer.failure != nil {
			return layer.failure
		}
	}
	return fallback(err)
}

func (c *layerChain) close() {
	for _, closer := range c.closers {
		_ = closer.Close()
	}
}

type trackedReader struct {
	reader   io.Reader
	classify func(error) error
	failure  error
}

func (r *trackedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && r.failure == nil {
		r.failure = r.classify(err)
	}
	return n, err
}

type trackedWriter struct {
	writer io.Writer
	err    error
}

func (w *trackedWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}

type contextReader struct {
	ctx    context.Context //nolint:containedctx // io.Reader has no way to receive a context.
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, fmt.Errorf("read backup: %w", err)
	}
	return r.reader.Read(p)
}
