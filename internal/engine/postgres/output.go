package postgres

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/backproofhq/runner/internal/job"
)

const maxLineLength = 64 * 1024

var (
	missingExtension = regexp.MustCompile(`extension "([A-Za-z0-9_-]+)" is not available`)
	missingControl   = regexp.MustCompile(`could not open extension control file ".*/([A-Za-z0-9_-]+)\.control"`)
	archiveVersion   = regexp.MustCompile(`unsupported version \((\d+\.\d+(?:\.\d+)?)\) in file header`)

	// These lines echo the failing statement, which can hold SQL and row contents from the dump.
	echoedPrefixes = []string{"LINE ", "DETAIL:", "HINT:", "QUERY:", "CONTEXT:", "STATEMENT:", "Command was:"}
)

// restoreOutput keeps conclusions about the restore tools' output and never the output itself.
type restoreOutput struct {
	diskFull       bool
	connectionLost bool
	extension      string
	archiveVersion string
}

func (o *restoreOutput) observe(line string) {
	trimmed := strings.TrimSpace(line)
	for _, prefix := range echoedPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return
		}
	}

	switch {
	case strings.Contains(line, "No space left on device"):
		o.diskFull = true
	case strings.Contains(line, "server closed the connection unexpectedly"),
		strings.Contains(line, "terminating connection due to"),
		strings.Contains(line, "connection to server"):
		o.connectionLost = true
	}
	if o.extension == "" {
		if match := missingExtension.FindStringSubmatch(line); match != nil {
			o.extension = match[1]
		} else if match := missingControl.FindStringSubmatch(line); match != nil {
			o.extension = match[1]
		}
	}
	if o.archiveVersion == "" {
		if match := archiveVersion.FindStringSubmatch(line); match != nil {
			o.archiveVersion = match[1]
		}
	}
}

func (o *restoreOutput) failure(major int) error {
	switch {
	case o.diskFull:
		return job.RunnerError("disk_full", "The Docker host ran out of disk space during the restore. Free some space and run it again.", nil)
	case o.connectionLost:
		return databaseStopped()
	case o.extension != "":
		return job.Fail("missing_extension", fmt.Sprintf("The backup needs the Postgres extension %q, which the postgres:%d image doesn't include.", o.extension, major))
	case o.archiveVersion != "":
		return job.Fail("dump_version_unsupported", fmt.Sprintf("Postgres %d can't read this backup's archive version %s. The backup was probably made with a newer Postgres version.", major, o.archiveVersion))
	default:
		return job.Fail("restore_failed", fmt.Sprintf("Postgres %d stopped the restore with an error. Restore the file by hand to see the details, which are left out here because they can contain backup data.", major))
	}
}

func databaseStopped() error {
	return job.RunnerError("database_stopped", "The throwaway Postgres server stopped during the restore. Check the memory and disk space on the Docker host, then run it again.", nil)
}

// lineWriter splits a stream into lines and skips lines too long to be tool messages.
type lineWriter struct {
	onLine   func(string)
	partial  []byte
	skipping bool
}

func newLineWriter(onLine func(string)) *lineWriter {
	return &lineWriter{onLine: onLine}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	written := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			w.keep(p)
			break
		}
		w.keep(p[:end])
		w.flush()
		p = p[end+1:]
	}
	return written, nil
}

func (w *lineWriter) keep(p []byte) {
	if w.skipping {
		return
	}
	if len(w.partial)+len(p) > maxLineLength {
		w.partial = w.partial[:0]
		w.skipping = true
		return
	}
	w.partial = append(w.partial, p...)
}

func (w *lineWriter) flush() {
	if !w.skipping && len(w.partial) > 0 {
		w.onLine(string(w.partial))
	}
	w.partial = w.partial[:0]
	w.skipping = false
}
