// Package source finds and downloads backups from S3-compatible storage.
package source

import (
	"context"
	"crypto/sha1" //nolint:gosec // Verifies a checksum the provider chose. It protects against damage, not attacks.
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/job"
)

const (
	downloadAttempts = 3
	clientRetries    = 3
)

var _ job.Source = (*Client)(nil)

// Config describes where a source keeps its backups.
type Config struct {
	ID       string
	Endpoint string
	Region   string
	Bucket   string
	Prefix   string
	// Pattern is a glob matched against the file name, such as "*.dump.gz". Empty matches every file.
	Pattern string
	// ObjectKey tests this exact backup instead of the newest one.
	ObjectKey string
	AccessKey config.Secret
	SecretKey config.Secret
}

// Client reads backups from one source.
type Client struct {
	config     Config
	api        *minio.Client
	setupErr   error
	retryDelay time.Duration
}

// New returns a client. Problems with the configuration are reported by Latest, so they end up in
// the job's result.
func New(cfg Config) *Client {
	client := &Client{config: cfg, retryDelay: time.Second}
	client.api, client.setupErr = connect(cfg)
	return client
}

func connect(cfg Config) (*minio.Client, error) {
	if !cfg.AccessKey.IsSet() || !cfg.SecretKey.IsSet() {
		return nil, job.RunnerError(
			"missing_credentials",
			fmt.Sprintf("No credentials for source %s on this runner. Enter its keys in the dashboard.", cfg.ID),
			nil,
		)
	}
	if _, err := path.Match(cfg.Pattern, ""); err != nil {
		return nil, job.RunnerError("invalid_pattern", fmt.Sprintf("The file pattern of source %s isn't valid. Correct it in the dashboard.", cfg.ID), nil)
	}

	invalidEndpoint := job.RunnerError("invalid_endpoint", fmt.Sprintf("The storage endpoint of source %s isn't valid. Correct it in the dashboard.", cfg.ID), nil)
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return nil, invalidEndpoint
	}
	api, err := minio.New(endpoint.Host, &minio.Options{
		Creds:      credentials.NewStaticV4(cfg.AccessKey.Reveal(), cfg.SecretKey.Reveal(), ""),
		Secure:     endpoint.Scheme == "https",
		Region:     cfg.Region,
		MaxRetries: clientRetries,
	})
	if err != nil {
		return nil, invalidEndpoint
	}
	return api, nil
}

// Latest returns the pinned backup, or the newest object that matches the source's pattern.
func (c *Client) Latest(ctx context.Context) (job.Object, error) {
	if c.setupErr != nil {
		return job.Object{}, c.setupErr
	}
	if c.config.ObjectKey != "" {
		return c.pinned(ctx)
	}

	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var newest job.Object
	for item := range c.api.ListObjects(listCtx, c.config.Bucket, minio.ListObjectsOptions{Prefix: c.config.Prefix, Recursive: true}) {
		if item.Err != nil {
			return job.Object{}, c.failure(item.Err)
		}
		if !c.matches(item.Key) {
			continue
		}
		candidate := job.Object{Key: item.Key, Size: item.Size, LastModified: item.LastModified}
		if isNewer(candidate, newest) {
			newest = candidate
		}
	}
	if newest.Key == "" {
		return job.Object{}, job.Fail("no_backup_found", "No backup found. Check that your backup job is running and that the prefix and file pattern are right.")
	}
	return newest, nil
}

func (c *Client) pinned(ctx context.Context) (job.Object, error) {
	info, err := c.api.StatObject(ctx, c.config.Bucket, c.config.ObjectKey, minio.StatObjectOptions{})
	if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
		return job.Object{}, job.Fail("no_backup_found", "No backup found. The chosen backup is no longer in the bucket.")
	}
	if err != nil {
		return job.Object{}, c.failure(err)
	}
	return job.Object{Key: info.Key, Size: info.Size, LastModified: info.LastModified}, nil
}

func (c *Client) matches(key string) bool {
	if strings.HasSuffix(key, "/") {
		return false
	}
	if c.config.Pattern == "" {
		return true
	}
	matched, err := path.Match(c.config.Pattern, path.Base(key))
	return err == nil && matched
}

// isNewer breaks ties on the key, so every runner picks the same backup.
func isNewer(candidate, current job.Object) bool {
	if candidate.LastModified.Equal(current.LastModified) {
		return candidate.Key > current.Key
	}
	return candidate.LastModified.After(current.LastModified)
}

// Download writes the object to path. Interrupted transfers are retried from the start.
func (c *Client) Download(ctx context.Context, object job.Object, target string) error {
	var err error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		err = c.downloadOnce(ctx, object, target)
		var transient *transientError
		if !errors.As(err, &transient) {
			return err
		}
		err = transient.failure
		if attempt == downloadAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait to retry download: %w", ctx.Err())
		case <-time.After(time.Duration(attempt) * c.retryDelay):
		}
	}
	return err
}

// transientError marks a failed transfer that another attempt may fix.
type transientError struct {
	failure error
}

func (e *transientError) Error() string {
	return e.failure.Error()
}

func (c *Client) downloadOnce(ctx context.Context, object job.Object, target string) error {
	reader, err := c.api.GetObject(ctx, c.config.Bucket, object.Key, minio.GetObjectOptions{Checksum: true})
	if err != nil {
		return c.failure(err)
	}
	defer func() { _ = reader.Close() }()

	info, err := reader.Stat()
	if err != nil {
		return c.failure(err)
	}

	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // The path is built from the runner's own work directory.
	if err != nil {
		return job.WorkFileFailure(err)
	}
	defer func() { _ = file.Close() }()

	checksums := checksumsOf(info)
	output := &trackedWriter{writer: file}
	writers := []io.Writer{output}
	for _, checksum := range checksums {
		writers = append(writers, checksum.hash)
	}

	written, err := io.Copy(io.MultiWriter(writers...), reader)
	if output.err != nil {
		return job.WorkFileFailure(output.err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("download backup: %w", ctx.Err())
	}
	if err != nil || written != info.Size {
		return &transientError{failure: job.RunnerError(
			"download_interrupted",
			"The download from the storage provider kept stopping before the end. Check the network and try again.",
			err,
		)}
	}
	for _, checksum := range checksums {
		if base64.StdEncoding.EncodeToString(checksum.hash.Sum(nil)) != checksum.want {
			return &transientError{failure: job.Fail(
				"checksum_mismatch",
				"The downloaded backup doesn't match the checksum stored with it, so the stored file is damaged. Check the job that creates your backups.",
			)}
		}
	}
	if err := file.Sync(); err != nil {
		return job.WorkFileFailure(err)
	}
	return nil
}

type checksum struct {
	want string
	hash hash.Hash
}

// checksumsOf skips multipart checksums, which cover the parts rather than the whole file.
func checksumsOf(info minio.ObjectInfo) []checksum {
	candidates := []struct {
		want string
		hash func() hash.Hash
	}{
		{info.ChecksumSHA256, sha256.New},
		{info.ChecksumSHA1, sha1.New},
		{info.ChecksumCRC32, func() hash.Hash { return crc32.NewIEEE() }},
		{info.ChecksumCRC32C, func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) }},
	}

	var checksums []checksum
	for _, candidate := range candidates {
		if candidate.want != "" && !strings.Contains(candidate.want, "-") {
			checksums = append(checksums, checksum{want: candidate.want, hash: candidate.hash()})
		}
	}
	return checksums
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

func (c *Client) failure(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("read from storage: %w", err)
	}
	switch minio.ToErrorResponse(err).Code {
	case minio.AccessDenied, minio.InvalidAccessKeyID, minio.SignatureDoesNotMatch, "InvalidToken", "ExpiredToken", "AuthorizationHeaderMalformed":
		return job.RunnerError("source_credentials", fmt.Sprintf("The storage provider rejected the keys for source %s. Check the keys and their permissions, then enter them again in the dashboard.", c.config.ID), err)
	case minio.NoSuchBucket:
		return job.RunnerError("source_bucket_missing", fmt.Sprintf("The bucket of source %s doesn't exist. Correct the bucket name in the dashboard.", c.config.ID), err)
	default:
		return job.RunnerError("source_unreachable", fmt.Sprintf("The runner couldn't read from the storage of source %s. Check the endpoint and the network, then try again.", c.config.ID), err)
	}
}
