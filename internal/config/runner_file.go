package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RunnerEnvFile is where registration stores the runner's settings.
const RunnerEnvFile = "/etc/backproof/runner.env"

// ErrRunnerFileExists means this machine has already registered a runner.
var ErrRunnerFileExists = errors.New("runner file exists")

// RunnerSettings are the values saved when a runner registers.
type RunnerSettings struct {
	Token     Secret
	RunnerKey Secret
	Name      string
	APIURL    string
}

// RunnerFile is a settings file that is reserved before registering. An install token works only
// once, so a directory that can't be written must be found before the token is spent.
type RunnerFile struct {
	path string
	temp *os.File
}

// ReserveRunnerFile checks that the settings can be saved at path.
func ReserveRunnerFile(path string) (*RunnerFile, error) {
	if _, err := os.Lstat(path); err == nil {
		return nil, ErrRunnerFileExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("check runner file: %w", err)
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create runner directory: %w", err)
	}
	// CreateTemp opens the file with mode 600, before any secret is written to it.
	temp, err := os.CreateTemp(directory, ".runner-env-*")
	if err != nil {
		return nil, fmt.Errorf("create runner file: %w", err)
	}
	return &RunnerFile{path: path, temp: temp}, nil
}

// Commit saves the settings. The result can be passed to docker run --env-file, which takes every
// line literally, so values are written without quotes.
func (f *RunnerFile) Commit(settings RunnerSettings) error {
	lines := []string{
		EnvToken + "=" + settings.Token.Reveal(),
		EnvRunnerKey + "=" + settings.RunnerKey.Reveal(),
		EnvName + "=" + settings.Name,
	}
	if settings.APIURL != DefaultAPIURL {
		lines = append(lines, EnvAPIURL+"="+settings.APIURL)
	}

	if _, err := f.temp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		return fmt.Errorf("write runner file: %w", err)
	}
	if err := f.temp.Sync(); err != nil {
		return fmt.Errorf("write runner file: %w", err)
	}
	if err := f.temp.Close(); err != nil {
		return fmt.Errorf("write runner file: %w", err)
	}
	// Link fails if the file appeared in the meantime, where a rename would replace it.
	if err := os.Link(f.temp.Name(), f.path); err != nil {
		return fmt.Errorf("save runner file: %w", err)
	}
	return nil
}

// Discard removes the reservation. It is safe to call after Commit.
func (f *RunnerFile) Discard() {
	_ = f.temp.Close()
	_ = os.Remove(f.temp.Name())
}
