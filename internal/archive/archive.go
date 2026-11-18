// Package archive identifies backup files from their contents.
package archive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// Format is the kind of file a backup turned out to be.
type Format string

// Formats the runner can recognise.
const (
	FormatEmpty   Format = "empty"
	FormatUnknown Format = "unknown"
	FormatCustom  Format = "custom"
	FormatPlain   Format = "plain"
	FormatGzip    Format = "gzip"
	FormatZstd    Format = "zstd"
	FormatAge     Format = "age"
	FormatPGP     Format = "pgp"
)

const (
	sniffSize         = 8 * 1024
	plainFooterWindow = 4 * 1024
	plainDumpHeader   = "-- PostgreSQL database dump"
	plainDumpFooter   = "-- PostgreSQL database dump complete"
)

var signatures = []struct {
	prefix []byte
	format Format
}{
	{[]byte("PGDMP"), FormatCustom},
	{[]byte{0x1f, 0x8b}, FormatGzip},
	{[]byte{0x28, 0xb5, 0x2f, 0xfd}, FormatZstd},
	{[]byte("age-encryption.org/"), FormatAge},
	{[]byte("-----BEGIN AGE ENCRYPTED FILE-----"), FormatAge},
	{[]byte("-----BEGIN PGP MESSAGE-----"), FormatPGP},
}

// Sniff detects the format from the first bytes of r, because file extensions can't be trusted.
func Sniff(r io.Reader) (Format, error) {
	sample := make([]byte, sniffSize)
	n, err := io.ReadFull(r, sample)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return FormatUnknown, fmt.Errorf("read backup header: %w", err)
	}
	sample = sample[:n]

	if len(sample) == 0 {
		return FormatEmpty, nil
	}
	for _, signature := range signatures {
		if bytes.HasPrefix(sample, signature.prefix) {
			return signature.format, nil
		}
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return FormatUnknown, nil
	}
	return FormatPlain, nil
}

// PlainDumpTruncated reports whether a pg_dump plain file lost its end. psql exits successfully on
// such a file, because it can't tell a short file from a finished one. Only pg_dump's own output
// has the footer, so SQL files without its header are never reported.
func PlainDumpTruncated(file io.ReaderAt, size int64) (bool, error) {
	head, err := readSection(file, 0, min(size, sniffSize))
	if err != nil {
		return false, fmt.Errorf("read backup header: %w", err)
	}
	if !bytes.Contains(head, []byte(plainDumpHeader)) {
		return false, nil
	}

	offset := max(size-plainFooterWindow, 0)
	tail, err := readSection(file, offset, size-offset)
	if err != nil {
		return false, fmt.Errorf("read backup footer: %w", err)
	}
	return !bytes.Contains(tail, []byte(plainDumpFooter)), nil
}

func readSection(file io.ReaderAt, offset, length int64) ([]byte, error) {
	section := make([]byte, length)
	n, err := file.ReadAt(section, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return section[:n], nil
}
