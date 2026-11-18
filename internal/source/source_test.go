package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/job"
)

const (
	testBucket    = "backups"
	testAccessKey = "test-access-key"
	testSecretKey = "test-secret-key-canary"
)

type storedObject struct {
	body     []byte
	modified time.Time
	headers  map[string]string
}

// fakeStorage speaks the small part of the S3 API that the runner uses.
type fakeStorage struct {
	mu         sync.Mutex
	objects    map[string]storedObject
	errorCode  string
	shortReads int
	gets       int
}

func (s *fakeStorage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.errorCode != "" {
		w.Header().Set("Content-Type", "application/xml")
		status := http.StatusForbidden
		if s.errorCode == "NoSuchBucket" {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `<Error><Code>%s</Code><Message>rejected</Message></Error>`, s.errorCode)
		return
	}

	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/"+testBucket), "/")
	if key == "" {
		s.list(w, r.URL.Query().Get("prefix"))
		return
	}
	object, ok := s.objects[key]
	if !ok {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
		return
	}

	w.Header().Set("ETag", `"etag"`)
	w.Header().Set("Last-Modified", object.modified.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", fmt.Sprint(len(object.body)))
	for name, value := range object.headers {
		w.Header().Set(name, value)
	}
	if r.Method == http.MethodHead {
		return
	}
	s.gets++
	if s.shortReads > 0 {
		s.shortReads--
		_, _ = w.Write(object.body[:len(object.body)/2])
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(object.body)
}

func (s *fakeStorage) list(w http.ResponseWriter, prefix string) {
	type content struct {
		Key          string
		LastModified string
		ETag         string
		Size         int
	}
	type listing struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		Name        string
		Prefix      string
		IsTruncated bool
		Contents    []content
	}

	result := listing{Name: testBucket, Prefix: prefix}
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.HasPrefix(key, prefix) {
			object := s.objects[key]
			result.Contents = append(result.Contents, content{
				Key:          key,
				LastModified: object.modified.UTC().Format("2006-01-02T15:04:05.000Z"),
				ETag:         `"etag"`,
				Size:         len(object.body),
			})
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

func newStorage(t *testing.T, objects map[string]storedObject) (*fakeStorage, Config) {
	t.Helper()
	storage := &fakeStorage{objects: objects}
	server := httptest.NewServer(storage)
	t.Cleanup(server.Close)
	return storage, Config{
		ID:        "PROD_DB",
		Endpoint:  server.URL,
		Region:    "us-east-1",
		Bucket:    testBucket,
		AccessKey: config.NewSecret(testAccessKey),
		SecretKey: config.NewSecret(testSecretKey),
	}
}

func newClient(cfg Config) *Client {
	client := New(cfg)
	client.retryDelay = time.Millisecond
	return client
}

// assertFailure returns the reason shown to the user.
func assertFailure(t *testing.T, err error, status job.Status, code string) string {
	t.Helper()
	var failure *job.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want a job failure", err)
	}
	if failure.Status != status || failure.Code != code {
		t.Errorf("failure = %s (%s), want %s (%s)", failure.Status, failure.Code, status, code)
	}
	if strings.Contains(failure.Error(), testSecretKey) {
		t.Errorf("reason leaked the secret key: %q", failure.Error())
	}
	return failure.Reason
}

func TestLatestPicksTheNewestMatchingObject(t *testing.T) {
	base := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	objects := map[string]storedObject{
		"db/daily-1.dump.gz":    {body: []byte("a"), modified: base},
		"db/daily-3.dump.gz":    {body: []byte("newest"), modified: base.Add(48 * time.Hour)},
		"db/daily-2.dump.gz":    {body: []byte("b"), modified: base.Add(24 * time.Hour)},
		"db/nested/z.dump.gz":   {body: []byte("c"), modified: base.Add(time.Hour)},
		"db/notes.txt":          {body: []byte("newer, but the wrong pattern"), modified: base.Add(72 * time.Hour)},
		"db/folder.dump.gz/":    {modified: base.Add(96 * time.Hour)},
		"other/daily-9.dump.gz": {body: []byte("newer, but the wrong prefix"), modified: base.Add(96 * time.Hour)},
	}
	_, cfg := newStorage(t, objects)
	cfg.Prefix = "db/"
	cfg.Pattern = "*.dump.gz"

	got, err := newClient(cfg).Latest(t.Context())
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if got.Key != "db/daily-3.dump.gz" || got.Size != int64(len("newest")) {
		t.Errorf("Latest() = %+v, want db/daily-3.dump.gz", got)
	}
}

func TestLatestBreaksTiesTheSameWayEveryTime(t *testing.T) {
	modified := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	_, cfg := newStorage(t, map[string]storedObject{
		"a.dump": {body: []byte("a"), modified: modified},
		"c.dump": {body: []byte("c"), modified: modified},
		"b.dump": {body: []byte("b"), modified: modified},
	})

	got, err := newClient(cfg).Latest(t.Context())
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if got.Key != "c.dump" {
		t.Errorf("Latest() = %s, want c.dump", got.Key)
	}
}

func TestLatestUsesThePinnedObject(t *testing.T) {
	base := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	_, cfg := newStorage(t, map[string]storedObject{
		"old.dump": {body: []byte("old"), modified: base},
		"new.dump": {body: []byte("new"), modified: base.Add(time.Hour)},
	})

	cfg.ObjectKey = "old.dump"
	got, err := newClient(cfg).Latest(t.Context())
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if got.Key != "old.dump" {
		t.Errorf("Latest() = %s, want the pinned object", got.Key)
	}

	cfg.ObjectKey = "deleted.dump"
	_, err = newClient(cfg).Latest(t.Context())
	assertFailure(t, err, job.StatusFail, "no_backup_found")
}

func TestLatestReportsWhatWentWrong(t *testing.T) {
	object := map[string]storedObject{"a.dump": {body: []byte("a"), modified: time.Now()}}

	t.Run("empty prefix is a bad backup", func(t *testing.T) {
		_, cfg := newStorage(t, object)
		cfg.Prefix = "nothing-here/"
		_, err := newClient(cfg).Latest(t.Context())
		_ = assertFailure(t, err, job.StatusFail, "no_backup_found")
	})
	t.Run("no file matches the pattern", func(t *testing.T) {
		_, cfg := newStorage(t, object)
		cfg.Pattern = "*.sql"
		_, err := newClient(cfg).Latest(t.Context())
		_ = assertFailure(t, err, job.StatusFail, "no_backup_found")
	})

	for code, want := range map[string]string{
		"AccessDenied":          "source_credentials",
		"InvalidAccessKeyId":    "source_credentials",
		"SignatureDoesNotMatch": "source_credentials",
		"NoSuchBucket":          "source_bucket_missing",
	} {
		t.Run(code, func(t *testing.T) {
			storage, cfg := newStorage(t, object)
			storage.errorCode = code
			_, err := newClient(cfg).Latest(t.Context())
			assertFailure(t, err, job.StatusError, want)
		})
	}

	t.Run("storage is unreachable", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // The test only needs a free port.
		if err != nil {
			t.Fatalf("find a free port: %v", err)
		}
		address := listener.Addr().String()
		_ = listener.Close()

		_, cfg := newStorage(t, object)
		cfg.Endpoint = "http://" + address
		_, err = newClient(cfg).Latest(t.Context())
		assertFailure(t, err, job.StatusError, "source_unreachable")
	})

	t.Run("no credentials", func(t *testing.T) {
		_, cfg := newStorage(t, object)
		cfg.SecretKey = config.Secret{}
		_, err := newClient(cfg).Latest(t.Context())
		reason := assertFailure(t, err, job.StatusError, "missing_credentials")
		if !strings.Contains(reason, "PROD_DB") {
			t.Errorf("reason %q does not name the source", reason)
		}
	})

	for name, change := range map[string]func(*Config){
		"invalid pattern":       func(c *Config) { c.Pattern = "[" },
		"endpoint without host": func(c *Config) { c.Endpoint = "https://" },
		"endpoint with keys":    func(c *Config) { c.Endpoint = "https://user:" + testSecretKey + "@example.test" },
		"unsupported scheme":    func(c *Config) { c.Endpoint = "ftp://example.test" },
	} {
		t.Run(name, func(t *testing.T) {
			_, cfg := newStorage(t, object)
			change(&cfg)
			_, err := newClient(cfg).Latest(t.Context())
			var failure *job.Failure
			if !errors.As(err, &failure) || failure.Status != job.StatusError {
				t.Fatalf("error = %v, want a runner error", err)
			}
			if strings.Contains(failure.Reason, testSecretKey) {
				t.Errorf("reason leaked the secret key: %q", failure.Reason)
			}
		})
	}
}

func download(t *testing.T, cfg Config, key string) (string, error) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "download")
	return target, newClient(cfg).Download(t.Context(), job.Object{Key: key}, target)
}

func sha256Header(body []byte) string {
	sum := sha256.Sum256(body)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func TestDownloadWritesAPrivateCopy(t *testing.T) {
	body := bytes.Repeat([]byte("backup"), 1000)
	_, cfg := newStorage(t, map[string]storedObject{"a.dump": {
		body:     body,
		modified: time.Now(),
		headers:  map[string]string{"x-amz-checksum-sha256": sha256Header(body)},
	}})

	target, err := download(t, cfg, "a.dump")
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	contents, err := os.ReadFile(target) //nolint:gosec // The path is inside the test's temp directory.
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if !bytes.Equal(contents, body) {
		t.Error("the download differs from the stored object")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat download: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("download has mode %o, want 600", info.Mode().Perm())
	}
}

func TestDownloadRetriesAnInterruptedTransfer(t *testing.T) {
	body := bytes.Repeat([]byte("backup"), 1000)
	storage, cfg := newStorage(t, map[string]storedObject{"a.dump": {body: body, modified: time.Now()}})
	storage.shortReads = downloadAttempts - 1

	target, err := download(t, cfg, "a.dump")
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	contents, err := os.ReadFile(target) //nolint:gosec // The path is inside the test's temp directory.
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if !bytes.Equal(contents, body) {
		t.Errorf("download has %d bytes, want the complete %d", len(contents), len(body))
	}
}

func TestDownloadThatKeepsFailingIsARunnerError(t *testing.T) {
	body := bytes.Repeat([]byte("backup"), 1000)
	storage, cfg := newStorage(t, map[string]storedObject{"a.dump": {body: body, modified: time.Now()}})
	storage.shortReads = 1000

	_, err := download(t, cfg, "a.dump")

	assertFailure(t, err, job.StatusError, "download_interrupted")
}

func TestDownloadRejectsAChecksumMismatch(t *testing.T) {
	body := bytes.Repeat([]byte("backup"), 1000)
	_, cfg := newStorage(t, map[string]storedObject{"a.dump": {
		body:     body,
		modified: time.Now(),
		headers:  map[string]string{"x-amz-checksum-sha256": sha256Header([]byte("something else"))},
	}})

	_, err := download(t, cfg, "a.dump")

	assertFailure(t, err, job.StatusFail, "checksum_mismatch")
}

func TestDownloadIgnoresMultipartChecksums(t *testing.T) {
	body := bytes.Repeat([]byte("backup"), 1000)
	_, cfg := newStorage(t, map[string]storedObject{"a.dump": {
		body:     body,
		modified: time.Now(),
		headers:  map[string]string{"x-amz-checksum-sha256": sha256Header([]byte("parts")) + "-3"},
	}})

	if _, err := download(t, cfg, "a.dump"); err != nil {
		t.Errorf("Download() error = %v, want multipart checksums to be skipped", err)
	}
}

func TestDownloadStopsWhenCancelled(t *testing.T) {
	body := bytes.Repeat([]byte("backup"), 1000)
	storage, cfg := newStorage(t, map[string]storedObject{"a.dump": {body: body, modified: time.Now()}})
	storage.shortReads = 1000

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := newClient(cfg).Download(ctx, job.Object{Key: "a.dump"}, filepath.Join(t.TempDir(), "download"))

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Download() error = %v, want it to report the cancellation", err)
	}
}
