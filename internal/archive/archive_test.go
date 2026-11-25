package archive

import (
	"bytes"
	"strings"
	"testing"
)

func TestPlainDumpTruncated(t *testing.T) {
	const header = "--\n-- PostgreSQL database dump\n--\n\nCREATE TABLE t (id int);\n"
	const footer = "\n--\n-- PostgreSQL database dump complete\n--\n\n"
	rows := strings.Repeat("INSERT INTO t VALUES (1);\n", 2000)

	tests := []struct {
		name string
		dump string
		want bool
	}{
		{"complete dump", header + rows + footer, false},
		{"complete dump with a closing command", header + rows + footer + "\\unrestrict abc123\n", false},
		{"cut before the footer", header + rows, true},
		{"cut inside the footer", header + rows + footer[:12], true},
		{"cut right after the header", header, true},
		{"footer text only in old row data", header + "-- PostgreSQL database dump complete\n" + rows, true},
		{"hand-written SQL has no footer to check", "CREATE TABLE t (id int);\n" + rows, false},
		{"cluster dump has a different format", "--\n-- PostgreSQL database cluster dump\n--\n" + rows, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := PlainDumpTruncated(strings.NewReader(test.dump), int64(len(test.dump)))
			if err != nil {
				t.Fatalf("PlainDumpTruncated() error = %v", err)
			}
			if got != test.want {
				t.Errorf("PlainDumpTruncated() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestSniff(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  Format
	}{
		{"empty file", nil, FormatEmpty},
		{"custom dump", []byte("PGDMP\x01\x10\x00"), FormatCustom},
		{"plain dump", []byte("--\n-- PostgreSQL database dump\n--\n"), FormatPlain},
		{"plain SQL without a header", []byte("CREATE TABLE t (id int);\n"), FormatPlain},
		{"latin-1 plain dump", []byte("INSERT INTO t VALUES ('caf\xe9');\n"), FormatPlain},
		{"gzip", []byte{0x1f, 0x8b, 0x08, 0x00}, FormatGzip},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd, 0x04}, FormatZstd},
		{"age", []byte("age-encryption.org/v1\n-> X25519"), FormatAge},
		{"armored age", []byte("-----BEGIN AGE ENCRYPTED FILE-----\n"), FormatAge},
		{"armored pgp", []byte("-----BEGIN PGP MESSAGE-----\n"), FormatPGP},
		{"binary pgp to a public key", []byte{0xc1, 0x4c, 0x03, 0xab, 0xcd}, FormatPGP},
		{"binary pgp in the old packet format", []byte{0x85, 0x01, 0x0c, 0x03, 0xab}, FormatPGP},
		{"binary pgp to a passphrase", []byte{0xc3, 0x0d, 0x04, 0x09, 0x03}, FormatPGP},
		{"binary data that resembles pgp", []byte{0xc1, 0x4c, 0x7f, 0x00, 0xcd}, FormatUnknown},
		{"binary data", []byte{0x00, 0x01, 0x02, 0xff}, FormatUnknown},
		{"zip", []byte("PK\x03\x04\x14\x00"), FormatZip},
		{"empty zip", []byte("PK\x05\x06\x00\x00"), FormatZip},
		{"posix tar", append(make([]byte, 257), "ustar\x0000"...), FormatTar},
		{"gnu tar", append(make([]byte, 257), "ustar  \x00"...), FormatTar},
		{"binary data shorter than a tar header", make([]byte, 200), FormatUnknown},
		{"text that turns binary", append([]byte(strings.Repeat("a", 100)), 0x00), FormatUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Sniff(bytes.NewReader(test.input))
			if err != nil {
				t.Fatalf("Sniff() error = %v", err)
			}
			if got != test.want {
				t.Errorf("Sniff() = %q, want %q", got, test.want)
			}
		})
	}
}
