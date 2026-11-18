// Package postgres restores Postgres dumps into throwaway containers.
package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/backproofhq/runner/internal/archive"
	"github.com/backproofhq/runner/internal/docker"
	"github.com/backproofhq/runner/internal/job"
)

// Postgres major versions the runner restores into.
const (
	OldestMajor = 14
	NewestMajor = 18
)

const (
	superuser      = "postgres"
	database       = "backproof_restore"
	startupTimeout = 2 * time.Minute
	killedBySignal = 128
)

var _ job.Engine = (*Engine)(nil)

// Engine restores Postgres dumps.
type Engine struct {
	docker *docker.Client
	limits docker.Limits
}

// New returns an engine that runs its containers through the given client, within the given limits.
func New(client *docker.Client, limits docker.Limits) *Engine {
	return &Engine{docker: client, limits: limits}
}

// Ready reports whether Docker can be used.
func (e *Engine) Ready(ctx context.Context) error {
	if err := e.docker.Ping(ctx); err != nil {
		return job.RunnerError("docker_unreachable", "Docker isn't reachable. Start Docker or mount its socket into the runner, then try again.", err)
	}
	return nil
}

// Cleanup removes every container and volume of the job.
func (e *Engine) Cleanup(ctx context.Context, jobID string) error {
	if err := e.docker.RemoveJob(ctx, jobID); err != nil {
		return fmt.Errorf("remove job %s: %w", jobID, err)
	}
	return nil
}

// DetectVersion reads the Postgres version from the dump, or falls back to the newest supported major.
func (e *Engine) DetectVersion(ctx context.Context, jobID string, dump job.Dump) (job.Version, error) {
	header, err := e.readHeader(ctx, jobID, dump)
	if err != nil {
		return job.Version{}, err
	}
	version, ok := parseVersion(dump.Format, header)
	if !ok {
		return job.Version{Major: NewestMajor, Source: job.VersionDefault}, nil
	}
	return version, nil
}

func (e *Engine) readHeader(ctx context.Context, jobID string, dump job.Dump) ([]byte, error) {
	file, err := openDump(dump)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	if dump.Format == archive.FormatPlain {
		header := make([]byte, headerSize)
		n, err := io.ReadFull(file, header)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, dumpUnreadable(err)
		}
		return header[:n], nil
	}

	// The newest pg_restore can read the table of contents of every older archive.
	helper, err := e.docker.Start(ctx, docker.ContainerSpec{
		JobID:      jobID,
		Image:      image(NewestMajor),
		Entrypoint: []string{"sleep"},
		Cmd:        []string{"infinity"},
		Limits:     e.limits,
	})
	if err != nil {
		return nil, dockerFailure(err)
	}
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = helper.Remove(removeCtx) // Cleanup removes anything left behind when the job ends.
	}()

	header := &prefixBuffer{limit: headerSize}
	_, err = helper.Exec(ctx, docker.ExecSpec{
		User:   superuser,
		Cmd:    []string{"pg_restore", "--list"},
		Stdin:  file,
		Stdout: header,
	})
	if err != nil {
		return nil, dockerFailure(err)
	}
	return header.Bytes(), nil
}

// Start launches the container that the dump is restored into.
func (e *Engine) Start(ctx context.Context, jobID string, version job.Version) (job.Instance, error) {
	major, err := restoreMajor(version.Major)
	if err != nil {
		return nil, err
	}

	container, err := e.docker.Start(ctx, docker.ContainerSpec{
		JobID: jobID,
		Image: image(major),
		// The container has no network, so nothing else can reach this database.
		Env:    []string{"POSTGRES_HOST_AUTH_METHOD=trust"},
		Limits: e.limits,
	})
	if err != nil {
		return nil, dockerFailure(err)
	}

	server := &Instance{container: container, major: major}
	if err := server.waitUntilReady(ctx); err != nil {
		return nil, err
	}
	code, err := server.exec(ctx, docker.ExecSpec{Cmd: []string{"createdb", "-U", superuser, "-T", "template0", database}})
	if err != nil {
		return nil, dockerFailure(err)
	}
	if code != 0 {
		return nil, job.RunnerError("database_start_failed", "The throwaway Postgres server couldn't create a database. Check the Docker host and try again.", nil)
	}
	return server, nil
}

func restoreMajor(dumpMajor int) (int, error) {
	switch {
	case dumpMajor > NewestMajor:
		return 0, job.Fail("dump_newer_than_supported", fmt.Sprintf(
			"This backup was made with Postgres %d, but the runner restores into Postgres %d at most. Update the runner to test it.",
			dumpMajor, NewestMajor,
		))
	case dumpMajor < OldestMajor:
		return OldestMajor, nil
	default:
		return dumpMajor, nil
	}
}

func image(major int) string {
	return "postgres:" + strconv.Itoa(major)
}

// Instance is a running throwaway Postgres server.
type Instance struct {
	container *docker.Container
	major     int
}

// Major returns the Postgres major version of the server.
func (i *Instance) Major() int {
	return i.major
}

func (i *Instance) exec(ctx context.Context, spec docker.ExecSpec) (int, error) {
	spec.User = superuser
	code, err := i.container.Exec(ctx, spec)
	if err != nil {
		return 0, fmt.Errorf("exec %s: %w", spec.Cmd[0], err)
	}
	return code, nil
}

// waitUntilReady checks over TCP because the image's first-boot server only listens on a socket.
func (i *Instance) waitUntilReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		code, err := i.exec(ctx, docker.ExecSpec{Cmd: []string{"pg_isready", "-q", "-h", "127.0.0.1", "-U", superuser}})
		if err == nil && code == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return job.RunnerError("database_start_failed", "The throwaway Postgres server didn't start in time. Check the Docker host and try again.", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Restore loads the dump into the throwaway database.
func (i *Instance) Restore(ctx context.Context, dump job.Dump) error {
	if err := i.createRoles(ctx, dump); err != nil {
		return err
	}

	file, err := openDump(dump)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	command := []string{"psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", superuser, "-d", database, "-f", "-"}
	if dump.Format == archive.FormatCustom {
		command = []string{"pg_restore", "--no-owner", "--no-acl", "--exit-on-error", "-U", superuser, "-d", database}
	}

	observed := &restoreOutput{}
	output := newLineWriter(observed.observe)
	code, err := i.exec(ctx, docker.ExecSpec{Cmd: command, Stdin: file, Stderr: output})
	if err != nil {
		return dockerFailure(err)
	}
	output.flush()
	// A tool killed by a signal says nothing about the backup, for example when memory ran out.
	if code >= killedBySignal {
		return databaseStopped()
	}
	if code != 0 {
		return observed.failure(i.major)
	}
	return nil
}

// createRoles adds the roles a dump refers to. They exist on the source server but aren't part of
// a pg_dump, so restoring without them would report a healthy backup as broken.
func (i *Instance) createRoles(ctx context.Context, dump job.Dump) error {
	file, err := openDump(dump)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	roles := newRoleScanner()
	lines := newLineWriter(roles.observe)
	if dump.Format == archive.FormatCustom {
		// A corrupt archive is reported by the restore itself, so the exit code doesn't matter here.
		_, err = i.exec(ctx, docker.ExecSpec{
			Cmd:    []string{"pg_restore", "--schema-only", "--no-owner", "--no-acl", "-f", "-"},
			Stdin:  file,
			Stdout: lines,
		})
		if err != nil {
			return dockerFailure(err)
		}
	} else if _, err := io.Copy(lines, file); err != nil {
		return dumpUnreadable(err)
	}
	lines.flush()

	script := roles.script()
	if len(script) == 0 {
		return nil
	}
	// One unusable role name must not stop the rest, so ON_ERROR_STOP stays off.
	_, err = i.exec(ctx, docker.ExecSpec{
		Cmd:   []string{"psql", "-X", "-q", "-U", superuser, "-d", database, "-f", "-"},
		Stdin: bytes.NewReader(script),
	})
	if err != nil {
		return dockerFailure(err)
	}
	return nil
}

func openDump(dump job.Dump) (*os.File, error) {
	file, err := os.Open(dump.Path)
	if err != nil {
		return nil, dumpUnreadable(err)
	}
	return file, nil
}

func dumpUnreadable(err error) error {
	return job.RunnerError("file_unreadable", "The backup file could not be read. Check the path and its permissions.", err)
}

func dockerFailure(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return job.RunnerError("docker_failed", "The runner couldn't control its Postgres container. Check that Docker is running and has free disk space, then try again.", err)
}

type prefixBuffer struct {
	limit  int
	buffer bytes.Buffer
}

// Write keeps the first bytes and accepts the rest, so a long listing doesn't stop the command.
func (b *prefixBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buffer.Len(); room > 0 {
		b.buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *prefixBuffer) Bytes() []byte {
	return b.buffer.Bytes()
}
