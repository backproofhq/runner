package postgres

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/backproofhq/runner/internal/archive"
	"github.com/backproofhq/runner/internal/job"
)

const headerSize = 64 * 1024

var (
	plainVersion  = regexp.MustCompile(`(?m)^-- Dumped from database version (\d+(?:\.\d+)*)`)
	customVersion = regexp.MustCompile(`(?m)^;\s+Dumped from database version: (\d+(?:\.\d+)*)`)
)

func parseVersion(format archive.Format, header []byte) (job.Version, bool) {
	pattern := plainVersion
	if format == archive.FormatCustom {
		pattern = customVersion
	}
	match := pattern.FindSubmatch(header)
	if match == nil {
		return job.Version{}, false
	}

	full := string(match[1])
	majorText, _, _ := strings.Cut(full, ".")
	major, err := strconv.Atoi(majorText)
	if err != nil {
		return job.Version{}, false
	}
	return job.Version{Full: full, Major: major, Source: job.VersionFromDump}, true
}
