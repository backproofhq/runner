package agent

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"filippo.io/age"
	"github.com/getkin/kin-openapi/openapi3"

	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/crypto"
)

const testRunnerID = "7b1f6c1e-3f0a-4a53-9a55-2f4f0e0a1c11"

// Tokens are generated when the tests run, so none are stored in the repository.
var (
	testInstallToken = "bp_inst_" + rand.Text()
	testRunnerToken  = "bp_run_" + rand.Text()
)

func contractSchema(t *testing.T, name string) *openapi3.Schema {
	t.Helper()
	document, err := openapi3.NewLoader().LoadFromFile("../../api/v1/openapi.yaml")
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	return document.Components.Schemas[name].Value
}

type fakeAPI struct {
	server   *httptest.Server
	requests atomic.Int32
	received map[string]any
}

// newFakeAPI answers as the contract says, and fails the test on a request that breaks it.
func newFakeAPI(t *testing.T, respond func(http.ResponseWriter)) *fakeAPI {
	t.Helper()
	schema := contractSchema(t, "RegisterRequest")
	fake := &fakeAPI{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runners/register" {
			t.Errorf("request = %s %s, want POST /v1/runners/register", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "backproof-runner/") {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		if err := json.Unmarshal(body, &fake.received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if err := schema.VisitJSON(fake.received); err != nil {
			t.Errorf("request breaks the contract: %v", err)
		}
		respond(w)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func registered(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = fmt.Fprintf(w, `{"runner_id":%q,"runner_token":%q}`, testRunnerID, testRunnerToken)
}

func register(t *testing.T, baseURL, path, name string) (Registration, error) {
	t.Helper()
	client, err := api.New(baseURL, "1.2.3")
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	return Register(t.Context(), client, RegisterOptions{
		InstallToken: config.NewSecret(testInstallToken),
		Name:         name,
		APIURL:       baseURL,
		FilePath:     path,
	})
}

func readSettings(t *testing.T, path string) map[string]string {
	t.Helper()
	contents, err := os.ReadFile(path) //nolint:gosec // The path is inside the test's temp directory.
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	settings := make(map[string]string)
	for line := range strings.Lines(string(contents)) {
		key, value, found := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
		if !found {
			t.Fatalf("settings line without a value: %q", key)
		}
		settings[key] = value
	}
	return settings
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("%d files left behind, want none", len(entries))
	}
}

func TestRegisterSavesTheCredentials(t *testing.T) {
	fake := newFakeAPI(t, registered)
	path := filepath.Join(t.TempDir(), "backproof", "runner.env")

	got, err := register(t, fake.server.URL, path, "build-box")
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	settings := readSettings(t, path)
	if settings[config.EnvToken] != testRunnerToken {
		t.Error("the runner token was not saved")
	}
	if settings[config.EnvName] != "build-box" {
		t.Errorf("name = %q, want build-box", settings[config.EnvName])
	}
	if settings[config.EnvAPIURL] != fake.server.URL {
		t.Errorf("API address = %q, want %q", settings[config.EnvAPIURL], fake.server.URL)
	}

	identity, err := age.ParseX25519Identity(settings[config.EnvRunnerKey])
	if err != nil {
		t.Fatalf("parse saved key: %v", err)
	}
	public := identity.Recipient().String()
	if fake.received["public_key"] != public {
		t.Error("the public key sent to the API doesn't belong to the saved private key")
	}
	if fake.received["install_token"] != testInstallToken || fake.received["name"] != "build-box" {
		t.Errorf("request = %v", fake.received)
	}
	if strings.Contains(fmt.Sprint(fake.received), settings[config.EnvRunnerKey]) {
		t.Error("the private key was sent to the API")
	}

	if got.RunnerID != testRunnerID || got.Name != "build-box" || got.Fingerprint != crypto.KeyFingerprint(public) {
		t.Errorf("Register() = %+v", got)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat settings: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("settings have mode %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("%d files in the directory, want only the settings", len(entries))
	}
}

func TestRegisterLeavesTheDefaultAddressOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.env")
	file, err := config.ReserveRunnerFile(path)
	if err != nil {
		t.Fatalf("reserve file: %v", err)
	}
	defer file.Discard()

	err = file.Commit(config.RunnerSettings{
		Token:     config.NewSecret(testRunnerToken),
		RunnerKey: config.NewSecret("test-key"),
		Name:      "box",
		APIURL:    config.DefaultAPIURL,
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if _, found := readSettings(t, path)[config.EnvAPIURL]; found {
		t.Error("the default API address was written to the settings")
	}
}

func TestRegisterSavesNothingWhenTheTokenIsRejected(t *testing.T) {
	const message = "This install token is invalid, expired or already used. Create a new one on the Add runner page."
	fake := newFakeAPI(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"code":"invalid_install_token","message":%q}`, message)
	})
	path := filepath.Join(t.TempDir(), "runner.env")

	_, err := register(t, fake.server.URL, path, "box")

	if err == nil || !strings.Contains(err.Error(), message) {
		t.Errorf("error = %v, want the API's explanation", err)
	}
	if strings.Contains(fmt.Sprint(err), testInstallToken) {
		t.Error("the error contains the install token")
	}
	assertDirectoryEmpty(t, path)
}

func TestRegisterKeepsAnExistingRunner(t *testing.T) {
	fake := newFakeAPI(t, registered)
	path := filepath.Join(t.TempDir(), "runner.env")
	if err := os.WriteFile(path, []byte("BACKPROOF_NAME=existing\n"), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	_, err := register(t, fake.server.URL, path, "box")

	if err == nil || !strings.Contains(err.Error(), "already has a registered runner") {
		t.Errorf("error = %v, want it to refuse", err)
	}
	if fake.requests.Load() != 0 {
		t.Error("the install token was spent even though the runner can't be saved")
	}
	if readSettings(t, path)[config.EnvName] != "existing" {
		t.Error("the existing settings were changed")
	}
}

func TestRegisterDoesNotSpendTheTokenWhenItCannotSave(t *testing.T) {
	fake := newFakeAPI(t, registered)
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := register(t, fake.server.URL, filepath.Join(blocked, "runner.env"), "box")

	if err == nil || !strings.Contains(err.Error(), "can't write") {
		t.Errorf("error = %v, want it to say the file can't be written", err)
	}
	if fake.requests.Load() != 0 {
		t.Error("the install token was spent even though the runner can't be saved")
	}
}

func TestRegisterRejectsAnUnusableToken(t *testing.T) {
	fake := newFakeAPI(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"runner_id":%q,"runner_token":"token\nBACKPROOF_API_URL=https://attacker.example.test"}`, testRunnerID)
	})
	path := filepath.Join(t.TempDir(), "runner.env")

	if _, err := register(t, fake.server.URL, path, "box"); err == nil {
		t.Fatal("Register() accepted a token with a line break")
	}
	assertDirectoryEmpty(t, path)
}

func TestRegisterDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	t.Cleanup(other.Close)
	fake := newFakeAPI(t, func(w http.ResponseWriter) {
		w.Header().Set("Location", other.URL+"/v1/runners/register")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	path := filepath.Join(t.TempDir(), "runner.env")

	if _, err := register(t, fake.server.URL, path, "box"); err == nil {
		t.Error("Register() succeeded after a redirect")
	}
	if redirected.Load() {
		t.Error("the install token was sent to the address in the redirect")
	}
	assertDirectoryEmpty(t, path)
}

func TestRegisterExplainsAnUnreachableAPI(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	address := server.URL
	server.Close()
	path := filepath.Join(t.TempDir(), "runner.env")

	_, err := register(t, address, path, "box")

	if err == nil || !strings.Contains(err.Error(), address) {
		t.Errorf("error = %v, want it to name the address", err)
	}
	assertDirectoryEmpty(t, path)
}

func TestRunnerName(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("read hostname: %v", err)
	}

	valid := map[string]string{
		"":                                 hostname,
		"build-box":                        "build-box",
		"Bouwserver #1":                    "Bouwserver #1",
		strings.Repeat("é", maxNameLength): strings.Repeat("é", maxNameLength),
	}
	for configured, want := range valid {
		got, err := runnerName(configured)
		if err != nil || got != want {
			t.Errorf("runnerName(%q) = %q, %v, want %q", configured, got, err, want)
		}
	}

	for _, name := range []string{"two\nlines", "box\rBACKPROOF_TOKEN=x", "tab\tname", "bad\xff", strings.Repeat("a", maxNameLength+1)} {
		if _, err := runnerName(name); err == nil {
			t.Errorf("runnerName(%q) accepted an invalid name", name)
		}
	}
}
