// Package docker runs the runner's throwaway containers and removes everything they leave behind.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// Labels carried by every container and volume the runner creates.
const (
	LabelRunner = "dev.backproof.runner"
	LabelJob    = "dev.backproof.job"
)

const networkNone = "none"

// Client creates and removes the containers of one runner.
type Client struct {
	api      *client.Client
	runnerID string
}

// New returns a client for the Docker daemon described by the environment.
func New(runnerID string) (*Client, error) {
	api, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &Client{api: api, runnerID: runnerID}, nil
}

// Close releases the connection to the Docker daemon.
func (c *Client) Close() error {
	if err := c.api.Close(); err != nil {
		return fmt.Errorf("close docker client: %w", err)
	}
	return nil
}

// Ping checks that the Docker daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.api.Ping(ctx, client.PingOptions{}); err != nil {
		return fmt.Errorf("ping docker: %w", err)
	}
	return nil
}

// Limits caps what a container may use. Zero means no limit.
type Limits struct {
	MemoryBytes int64
	NanoCPUs    int64
}

// ContainerSpec describes a throwaway container.
type ContainerSpec struct {
	JobID      string
	Image      string
	Env        []string
	Entrypoint []string
	Cmd        []string
	Limits     Limits
}

// Container is a running throwaway container.
type Container struct {
	client  *Client
	id      string
	volumes []string
}

// Start pulls the image if needed and starts an isolated container.
// RemoveJob cleans up whatever exists if Start fails part of the way through.
func (c *Client) Start(ctx context.Context, spec ContainerSpec) (*Container, error) {
	paths, err := c.imageVolumes(ctx, spec.Image)
	if err != nil {
		return nil, err
	}

	labels := c.labels(spec.JobID)
	created := &Container{client: c}
	mounts := make([]mount.Mount, 0, len(paths))
	// A named volume on every declared path stops Docker from creating unlabelled anonymous ones.
	for _, path := range paths {
		volume, err := c.api.VolumeCreate(ctx, client.VolumeCreateOptions{Labels: labels})
		if err != nil {
			return nil, fmt.Errorf("create volume for %s: %w", spec.Image, err)
		}
		created.volumes = append(created.volumes, volume.Volume.Name)
		mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: volume.Volume.Name, Target: path})
	}

	response, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      spec.Image,
			Env:        spec.Env,
			Entrypoint: spec.Entrypoint,
			Cmd:        spec.Cmd,
			Labels:     labels,
		},
		HostConfig: &container.HostConfig{
			NetworkMode: networkNone,
			Mounts:      mounts,
			Resources: container.Resources{
				Memory: spec.Limits.MemoryBytes,
				// Equal to Memory, because Docker otherwise allows the same amount again as swap.
				MemorySwap: spec.Limits.MemoryBytes,
				NanoCPUs:   spec.Limits.NanoCPUs,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create container from %s: %w", spec.Image, err)
	}
	created.id = response.ID

	if _, err := c.api.ContainerStart(ctx, created.id, client.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("start container from %s: %w", spec.Image, err)
	}
	return created, nil
}

func (c *Client) imageVolumes(ctx context.Context, image string) ([]string, error) {
	inspected, err := c.api.ImageInspect(ctx, image)
	if errdefs.IsNotFound(err) {
		pull, pullErr := c.api.ImagePull(ctx, image, client.ImagePullOptions{})
		if pullErr != nil {
			return nil, fmt.Errorf("pull %s: %w", image, pullErr)
		}
		defer func() { _ = pull.Close() }()
		if waitErr := pull.Wait(ctx); waitErr != nil {
			return nil, fmt.Errorf("pull %s: %w", image, waitErr)
		}
		inspected, err = c.api.ImageInspect(ctx, image)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", image, err)
	}
	if inspected.Config == nil {
		return nil, nil
	}

	paths := make([]string, 0, len(inspected.Config.Volumes))
	for path := range inspected.Config.Volumes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func (c *Client) labels(jobID string) map[string]string {
	return map[string]string{LabelRunner: c.runnerID, LabelJob: jobID}
}

// ExecSpec describes a command to run inside a container.
type ExecSpec struct {
	User   string
	Cmd    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Exec runs a command to completion and returns its exit code.
func (c *Container) Exec(ctx context.Context, spec ExecSpec) (int, error) {
	created, err := c.client.api.ExecCreate(ctx, c.id, client.ExecCreateOptions{
		User:         spec.User,
		Cmd:          spec.Cmd,
		AttachStdin:  spec.Stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return 0, fmt.Errorf("create exec: %w", err)
	}
	attach, err := c.client.api.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return 0, fmt.Errorf("attach exec: %w", err)
	}
	defer attach.Close()

	input := &trackedReader{reader: spec.Stdin}
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		if spec.Stdin == nil {
			return
		}
		// Write errors are expected when a command exits before reading all of its input.
		_, _ = io.Copy(attach.Conn, input)
		_ = attach.CloseWrite()
	}()

	var outputErr error
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		_, outputErr = stdcopy.StdCopy(orDiscard(spec.Stdout), orDiscard(spec.Stderr), attach.Reader)
	}()

	select {
	case <-ctx.Done():
		attach.Close()
		<-outputDone
		<-inputDone
		return 0, fmt.Errorf("run exec: %w", ctx.Err())
	case <-outputDone:
		attach.Close()
		<-inputDone
	}
	if outputErr != nil {
		return 0, fmt.Errorf("read exec output: %w", outputErr)
	}

	// A failed read looks like a short file to the command, which could report a false success.
	if input.err != nil {
		return 0, fmt.Errorf("read exec input: %w", input.err)
	}
	return c.exitCode(ctx, created.ID)
}

func (c *Container) exitCode(ctx context.Context, execID string) (int, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		inspected, err := c.client.api.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return 0, fmt.Errorf("inspect exec: %w", err)
		}
		if !inspected.Running {
			return inspected.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("wait for exec: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Remove deletes this container and its volumes.
func (c *Container) Remove(ctx context.Context) error {
	var errs []error
	if c.id != "" {
		_, err := c.client.api.ContainerRemove(ctx, c.id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove container: %w", err))
		}
	}
	for _, name := range c.volumes {
		_, err := c.client.api.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
		if err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove volume: %w", err))
		}
	}
	return errors.Join(errs...)
}

// RemoveJob deletes every container and volume labelled with this runner and job.
func (c *Client) RemoveJob(ctx context.Context, jobID string) error {
	return c.remove(ctx, make(client.Filters).Add("label", LabelRunner+"="+c.runnerID, LabelJob+"="+jobID), nil)
}

// RemoveOrphans deletes everything labelled with this runner, except what belongs to the given jobs.
// Other runners on the same Docker host keep their containers.
func (c *Client) RemoveOrphans(ctx context.Context, keepJobIDs ...string) error {
	return c.remove(ctx, make(client.Filters).Add("label", LabelRunner+"="+c.runnerID), keepJobIDs)
}

func (c *Client) remove(ctx context.Context, filters client.Filters, keepJobIDs []string) error {
	var errs []error
	containers, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		errs = append(errs, fmt.Errorf("list containers: %w", err))
	}
	for _, item := range containers.Items {
		if slices.Contains(keepJobIDs, item.Labels[LabelJob]) {
			continue
		}
		_, err := c.api.ContainerRemove(ctx, item.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove container: %w", err))
		}
	}

	volumes, err := c.api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		errs = append(errs, fmt.Errorf("list volumes: %w", err))
	}
	for _, item := range volumes.Items {
		if slices.Contains(keepJobIDs, item.Labels[LabelJob]) {
			continue
		}
		_, err := c.api.VolumeRemove(ctx, item.Name, client.VolumeRemoveOptions{Force: true})
		if err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove volume: %w", err))
		}
	}
	return errors.Join(errs...)
}

type trackedReader struct {
	reader io.Reader
	err    error
}

func (r *trackedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
