// Package archive identifies backup files from their contents.
package archive

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
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
	FormatZip     Format = "zip"
	FormatTar     Format = "tar"
)

const (
	// SniffSize is how many bytes SniffBytes needs to recognise a format.
	SniffSize         = 8 * 1024
	plainFooterWindow = 4 * 1024
	plainDumpHeader   = "-- PostgreSQL database dump"
	plainDumpFooter   = "-- PostgreSQL database dump complete"
	tarMagicOffset    = 257
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
	{[]byte("PK\x03\x04"), FormatZip},
	// An empty zip has only its end record.
	{[]byte("PK\x05\x06"), FormatZip},
}

// Sniff detects the format from the first bytes of r, because file extensions can't be trusted.
func Sniff(r io.Reader) (Format, error) {
	sample := make([]byte, SniffSize)
	n, err := io.ReadFull(r, sample)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return FormatUnknown, fmt.Errorf("read backup header: %w", err)
	}
	return SniffBytes(sample[:n]), nil
}

// SniffBytes detects the format from the first bytes of a file.
func SniffBytes(sample []byte) Format {
	if len(sample) == 0 {
		return FormatEmpty
	}
	for _, signature := range signatures {
		if bytes.HasPrefix(sample, signature.prefix) {
			return signature.format
		}
	}
	if isBinaryPGP(sample) {
		return FormatPGP
	}
	// POSIX and GNU tar both write "ustar" here. Older tar files without it are rare.
	if bytes.HasPrefix(sample[min(len(sample), tarMagicOffset):], []byte("ustar")) {
		return FormatTar
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return FormatUnknown
	}
	return FormatPlain
}

// isBinaryPGP looks for the packet that starts every encrypted OpenPGP message: a session key
// encrypted to a public key (tag 1) or a passphrase (tag 3), followed by a known packet version.
func isBinaryPGP(sample []byte) bool {
	if len(sample) < 4 || sample[0]&0x80 == 0 {
		return false
	}

	var tag byte
	var header int
	if sample[0]&0x40 != 0 {
		tag = sample[0] & 0x3f
		header = 2
		if sample[1] >= 192 && sample[1] < 224 {
			header = 3
		}
	} else {
		tag = sample[0] >> 2 & 0x0f
		header = 1 << (sample[0] & 0x03)
		header++
	}
	if header >= len(sample) {
		return false
	}

	version := sample[header]
	switch tag {
	case 1:
		return version == 3 || version == 6
	case 3:
		return version == 4 || version == 5 || version == 6
	default:
		return false
	}
}

// Decompress returns a reader for gzip or zstd data. Damaged data is reported while reading.
func Decompress(format Format, compressed io.Reader) (io.ReadCloser, error) {
	switch format {
	case FormatGzip:
		reader, err := gzip.NewReader(compressed)
		if err != nil {
			return nil, fmt.Errorf("read gzip header: %w", err)
		}
		return reader, nil
	case FormatZstd:
		reader, err := zstd.NewReader(compressed)
		if err != nil {
			return nil, fmt.Errorf("read zstd header: %w", err)
		}
		return reader.IOReadCloser(), nil
	default:
		return nil, fmt.Errorf("format %s is not compressed", format)
	}
}

// HasPlainDumpHeader reports whether a plain SQL file starts the way pg_dump writes it. Archives
// hold many text files, so only these count as dumps there.
func HasPlainDumpHeader(sample []byte) bool {
	return bytes.Contains(sample, []byte(plainDumpHeader))
}

// PlainDumpTruncated reports whether a pg_dump plain file lost its end. psql exits successfully on
// such a file, because it can't tell a short file from a finished one. Only pg_dump's own output
// has the footer, so SQL files without its header are never reported.
func PlainDumpTruncated(file io.ReaderAt, size int64) (bool, error) {
	head, err := readSection(file, 0, min(size, SniffSize))
	if err != nil {
		return false, fmt.Errorf("read backup header: %w", err)
	}
	if !HasPlainDumpHeader(head) {
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
