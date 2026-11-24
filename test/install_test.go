//go:build integration

package test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/crypto"
)

const (
	dockerCLIImage  = "docker:29.10.0-cli@sha256:34507eb5069d8be82c95dd3b6987d4250fd5fee49b82e2509b7d7a7030740d6d"
	dockerDindImage = "docker:29.10.0-dind@sha256:4bbc7223f81db790097c6dea42fb93e05b8fead4f0431fbe6ccaba6e8473201c"
	registryImage   = "registry:3@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c"
	debianImage     = "debian:13-slim@sha256:8aa5a7bf5a1955bddbe7d74fcdd0b91ab30e74362a8b73af672b0481b35b44a0"
	installVersion  = "0.1.0"
)

// installAPI stands in for Backproof during an install: it registers one runner and accepts its
// check-ins, or rejects its token when told to.
type installAPI struct {
	t            *testing.T
	schemas      openapi3.Schemas
	installToken string
	runnerToken  string

	mu          sync.Mutex
	rejectToken bool
	registered  []api.RegisterRequest
	requests    int
}

func (s *installAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Backproof-Latest-Version", installVersion)
	w.Header().Set("Backproof-Min-Version", "0.0.0")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runners/register":
		var request api.RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			s.t.Errorf("decode registration: %v", err)
		}
		if request.InstallToken != s.installToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":"invalid_install_token","message":"This install token is not valid. Create a new one on the Add runner page."}`)
			return
		}
		s.registered = append(s.registered, request)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"runner_id":"7b1f6c1e-3f0a-4a53-9a55-2f4f0e0a1c11","runner_token":%q}`, s.runnerToken)
	case s.rejectToken || r.Header.Get("Authorization") != "Bearer "+s.runnerToken:
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"invalid_token","message":"This runner's token was revoked. Register the runner again with a new install token."}`)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runners/poll":
		w.Header().Set("Backproof-Next-Poll-After", "1")
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/runners/me":
		fingerprint := ""
		if len(s.registered) > 0 {
			fingerprint = crypto.KeyFingerprint(s.registered[len(s.registered)-1].PublicKey)
		}
		_, _ = fmt.Fprintf(w, `{"runner_id":"7b1f6c1e-3f0a-4a53-9a55-2f4f0e0a1c11","name":"build-box","key_fingerprint":%q,"sources":[]}`, fingerprint)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// installHost is a Docker-in-Docker container that plays a fresh Linux server. Its Docker pulls
// backproofhq/runner from a local registry, which holds a runner built from this checkout.
type installHost struct {
	t      *testing.T
	name   string
	apiURL string
	script []byte
}

func startInstallHost(t *testing.T, handler http.Handler) *installHost {
	t.Helper()
	id := "bp-install-" + strings.ToLower(randomID(t))
	labels := []string{"--label", "dev.backproof.runner=" + id, "--label", "dev.backproof.job=" + id}
	registry := id + "-registry"
	image := id + ":runner"
	t.Cleanup(func() {
		for _, args := range [][]string{
			{"rm", "-fv", id}, {"rm", "-fv", registry}, {"volume", "rm", "-f", id}, {"network", "rm", id}, {"image", "rm", "-f", image},
		} {
			_ = dockerCommand(context.Background(), args...).Run()
		}
	})

	resolved := hostDocker(t, "run", "--rm", "--add-host", "host.docker.internal:host-gateway", "--entrypoint", "getent", dockerCLIImage, "ahostsv4", "host.docker.internal")
	hostIP := strings.Fields(resolved)[0]
	authority, certificate := newTestAuthority(t, hostIP)
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)

	buildRunnerImage(t, image, authority)
	hostDocker(t, append([]string{"network", "create"}, append(labels, id)...)...)
	hostDocker(t, append([]string{"volume", "create"}, append(labels, id)...)...)
	hostDocker(t, append(append([]string{"run", "--detach", "--name", registry, "--network", id, "--network-alias", "registry"}, labels...), registryImage)...)
	hostDocker(t, append(append([]string{"run", "--detach", "--privileged", "--name", id, "--hostname", "build-box",
		"--network", id, "--add-host", "host.docker.internal:host-gateway",
		"--env", "DOCKER_TLS_CERTDIR=/certs", "--volume", id + ":/certs/client"}, labels...),
		dockerDindImage, "--registry-mirror=http://registry:5000", "--insecure-registry=registry:5000")...)

	host := &installHost{t: t, name: id}
	deadline := time.Now().Add(2 * time.Minute)
	for dockerCommand(t.Context(), "exec", id, "docker", "info").Run() != nil {
		if time.Now().After(deadline) {
			t.Fatal("the Docker-in-Docker host didn't start")
		}
		time.Sleep(time.Second)
	}
	load := exec.CommandContext(t.Context(), "sh", "-c", `docker save "$1" | docker exec -i "$2" docker load`, "sh", image, id) //nolint:gosec // The test chooses both names.
	if output, err := load.CombinedOutput(); err != nil {
		t.Fatalf("load image: %v\n%s", err, output)
	}
	// The script pulls, so the runner image must come from the registry, not the host's cache.
	for _, tag := range []string{"latest", installVersion} {
		mirrored := "registry:5000/backproofhq/runner:" + tag
		host.docker("tag", image, mirrored)
		host.docker("push", mirrored)
		host.docker("image", "rm", mirrored)
	}
	host.docker("image", "rm", image)

	script, err := os.ReadFile("../scripts/install.sh")
	if err != nil {
		t.Fatalf("read install script: %v", err)
	}
	host.script = script
	host.apiURL = "https://" + hostIP + ":" + fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	return host
}

// dockerCommand runs the Docker CLI on the machine running the tests.
func dockerCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "docker", args...) //nolint:gosec // The test chooses every argument.
}

func hostDocker(t *testing.T, args ...string) string {
	t.Helper()
	output, err := dockerCommand(t.Context(), args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func newTestAuthority(t *testing.T, ip string) ([]byte, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Backproof install test"},
		IPAddresses: []net.IP{net.ParseIP(ip)},
		NotBefore:   time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// buildRunnerImage builds the release Dockerfile with a binary from this checkout, trusting only
// the test's certificate authority.
func buildRunnerImage(t *testing.T, image string, authority []byte) {
	t.Helper()
	context := t.TempDir()
	binary := filepath.Join(context, "linux", runtime.GOARCH, "backproof")
	build := exec.CommandContext(t.Context(), "go", "build", "-ldflags", "-X github.com/backproofhq/runner/internal/version.version="+installVersion, "-o", binary, "./cmd/backproof") //nolint:gosec // The output path is the test's own temp directory.
	build.Dir = ".."
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	dockerfile, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile = append(dockerfile, "\nCOPY ca.crt /etc/ssl/certs/ca-certificates.crt\n"...)
	for name, contents := range map[string][]byte{"Dockerfile": dockerfile, "ca.crt": authority} {
		if err := os.WriteFile(filepath.Join(context, name), contents, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	hostDocker(t, "build", "--quiet", "--tag", image, context)
}

func (h *installHost) docker(args ...string) string {
	h.t.Helper()
	return hostDocker(h.t, append([]string{"exec", h.name, "docker"}, args...)...)
}

// install pipes the script into sh, as curl does, without a terminal.
func (h *installHost) install(args ...string) (int, string) {
	h.t.Helper()
	command := dockerCommand(h.t.Context(), append([]string{"exec", "-i", "--env", "BACKPROOF_API_URL=" + h.apiURL, h.name, "sh", "-s", "--"}, args...)...)
	command.Stdin = bytes.NewReader(h.script)
	return exitCode(h.t, command)
}

func (h *installHost) exists(path string) bool {
	return dockerCommand(h.t.Context(), "exec", h.name, "test", "-e", path).Run() == nil
}

func exitCode(t *testing.T, command *exec.Cmd) (int, string) {
	t.Helper()
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), string(output)
	}
	if err != nil {
		t.Fatalf("run %s: %v", command.Args[0], err)
	}
	return 0, string(output)
}

func assertContains(t *testing.T, output string, want ...string) {
	t.Helper()
	for _, line := range want {
		if !strings.Contains(output, line) {
			t.Errorf("output is missing %q\n%s", line, output)
		}
	}
}

func TestInstallScriptInstallsAndRemovesTheRunner(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("../api/v1/openapi.yaml")
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	backproof := &installAPI{
		t:            t,
		schemas:      document.Components.Schemas,
		installToken: "bp_inst_" + randomID(t),
		runnerToken:  "bp_run_" + randomID(t),
	}
	host := startInstallHost(t, backproof)
	token := backproof.installToken
	requests := func() int {
		backproof.mu.Lock()
		defer backproof.mu.Unlock()
		return backproof.requests
	}
	assertNoRunner := func(t *testing.T) {
		t.Helper()
		if containers := host.docker("ps", "--all", "--quiet"); strings.TrimSpace(containers) != "" {
			t.Errorf("containers are left: %s", containers)
		}
		if volumes := host.docker("volume", "ls", "--quiet"); strings.TrimSpace(volumes) != "" {
			t.Errorf("volumes are left: %s", volumes)
		}
		if host.exists("/etc/backproof") {
			t.Error("/etc/backproof is left")
		}
	}

	t.Run("a dry run changes nothing", func(t *testing.T) {
		code, output := host.install("--token", token, "--dry-run")
		if code != 0 {
			t.Fatalf("exit code = %d\n%s", code, output)
		}
		assertContains(t, output,
			"Dry run: nothing on this host will change.",
			"Would run: docker pull backproofhq/runner:latest",
			"register --token '<install token>'",
			"Would ask: Start the runner container now? [Y/n]",
			"--restart unless-stopped --stop-timeout 1800",
		)
		if strings.Contains(output, token) {
			t.Errorf("the dry run printed the install token\n%s", output)
		}
		if images := host.docker("image", "ls", "--quiet"); strings.TrimSpace(images) != "" {
			t.Errorf("the dry run pulled images: %s", images)
		}
		assertNoRunner(t)
		if got := requests(); got != 0 {
			t.Errorf("the dry run sent %d requests to Backproof", got)
		}
	})

	t.Run("without a terminal or --yes it stops before changing anything", func(t *testing.T) {
		code, output := host.install("--token", token)
		if code != 1 {
			t.Errorf("exit code = %d, want 1\n%s", code, output)
		}
		assertContains(t, output, "There is no terminal to answer prompts")
		assertNoRunner(t)
		if got := requests(); got != 0 {
			t.Errorf("%d requests were sent to Backproof", got)
		}
	})

	t.Run("the one-liner registers and starts the runner", func(t *testing.T) {
		code, output := host.install("--token", token, "--yes", "--drain-timeout", "45m")
		if code != 0 {
			t.Fatalf("exit code = %d\n%s", code, output)
		}
		assertContains(t, output, "Installing backproof "+installVersion, "Runner connected.")
		if strings.Contains(output, token) || strings.Contains(output, backproof.runnerToken) {
			t.Errorf("the script printed a token\n%s", output)
		}

		backproof.mu.Lock()
		registered := backproof.registered
		backproof.mu.Unlock()
		if len(registered) != 1 || registered[0].Name != "build-box" {
			t.Errorf("registrations = %+v, want one named after the host", registered)
		}
		if mode := hostDocker(t, "exec", host.name, "stat", "-c", "%a %a", "/etc/backproof", "/etc/backproof/runner.env"); strings.Fields(mode)[0] != "700" || strings.Fields(mode)[2] != "600" {
			t.Errorf("modes of /etc/backproof and runner.env = %q, want 700 and 600", mode)
		}

		settings := host.docker("inspect", "--format", "{{.HostConfig.RestartPolicy.Name}} {{.Config.StopTimeout}} {{range .Mounts}}{{.Destination}}:{{.RW}} {{end}}", "backproof-runner")
		for _, want := range []string{"unless-stopped", " 2700 ", "/var/run/docker.sock:true", "/var/lib/backproof:true", "/etc/backproof:false"} {
			if !strings.Contains(" "+settings, want) {
				t.Errorf("runner container settings %q are missing %q", settings, want)
			}
		}
		// Settings come from the read-only mount, so the token isn't in the container's configuration.
		if env := host.docker("inspect", "--format", "{{json .Config.Env}}", "backproof-runner"); strings.Contains(env, "bp_run_") {
			t.Error("the runner token is in the container's environment")
		}
	})

	t.Run("running it again doesn't register a second runner", func(t *testing.T) {
		code, output := host.install("--token", token, "--yes")
		if code != 1 {
			t.Errorf("exit code = %d, want 1\n%s", code, output)
		}
		assertContains(t, output, "already installed")
		backproof.mu.Lock()
		defer backproof.mu.Unlock()
		if len(backproof.registered) != 1 {
			t.Errorf("%d registrations, want 1", len(backproof.registered))
		}
	})

	t.Run("uninstall removes the runner and what it left behind", func(t *testing.T) {
		host.docker("create", "--label", "dev.backproof.runner=leftover", "--label", "dev.backproof.job=leftover", "backproofhq/runner:latest", "version")
		host.docker("volume", "create", "--label", "dev.backproof.runner=leftover", "--label", "dev.backproof.job=leftover")
		code, output := host.install("--uninstall", "--yes")
		if code != 0 {
			t.Fatalf("exit code = %d\n%s", code, output)
		}
		assertContains(t, output, "The runner is removed from this host.")
		assertNoRunner(t)
	})

	t.Run("a runner that can't check in gets doctor's findings", func(t *testing.T) {
		backproof.mu.Lock()
		backproof.rejectToken = true
		backproof.mu.Unlock()

		code, output := host.install("--token", token, "--yes", "--version", "v"+installVersion, "--name", "edge-box")
		if code != 1 {
			t.Errorf("exit code = %d, want 1\n%s", code, output)
		}
		assertContains(t, output,
			"The runner didn't check in within 60 seconds.",
			"fail  Backproof API: This runner's token was revoked.",
			"docker logs backproof-runner",
		)
		if image := host.docker("inspect", "--format", "{{.Config.Image}}", "backproof-runner"); strings.TrimSpace(image) != "backproofhq/runner:"+installVersion {
			t.Errorf("runner image = %q, want backproofhq/runner:%s", image, installVersion)
		}
		backproof.mu.Lock()
		last := backproof.registered[len(backproof.registered)-1]
		backproof.mu.Unlock()
		if last.Name != "edge-box" {
			t.Errorf("registered name = %q, want edge-box", last.Name)
		}

		if code, output := host.install("--uninstall", "--yes", "--drain-timeout", "5s"); code != 0 {
			t.Fatalf("uninstall exit code = %d\n%s", code, output)
		}
		assertNoRunner(t)
	})
}

func TestInstallScriptOnAHostWithoutDocker(t *testing.T) {
	script, err := filepath.Abs("../scripts/install.sh")
	if err != nil {
		t.Fatalf("find install script: %v", err)
	}
	id := "bp-install-" + strings.ToLower(randomID(t))
	run := func(t *testing.T, stdin string, command ...string) (int, string) {
		t.Helper()
		args := append([]string{"run", "--rm", "-i", "--network", "none",
			"--label", "dev.backproof.runner=" + id, "--label", "dev.backproof.job=" + id,
			"--volume", script + ":/install.sh:ro", debianImage}, command...)
		docker := dockerCommand(t.Context(), args...)
		docker.Stdin = strings.NewReader(stdin)
		return exitCode(t, docker)
	}

	t.Run("answering no exits with install instructions", func(t *testing.T) {
		// script gives the installer a terminal, so the prompt reads the answer from /dev/tty.
		code, output := run(t, "n\n", "script", "--quiet", "--return", "--command", "sh /install.sh --token bp_inst_test", "/dev/null")
		if code != 1 {
			t.Errorf("exit code = %d, want 1\n%s", code, output)
		}
		assertContains(t, output,
			"Docker isn't installed. Install it now with Docker's official script (get.docker.com)? [y/N]",
			"Install Docker, then run this script again: https://docs.docker.com/engine/install/",
		)
	})

	t.Run("a dry run describes installing Docker", func(t *testing.T) {
		// The image has no curl, so the dry run also shows that a real run would stop for it.
		code, output := run(t, "", "sh", "/install.sh", "--token", "bp_inst_test", "--dry-run")
		if code != 0 {
			t.Errorf("exit code = %d, want 0\n%s", code, output)
		}
		assertContains(t, output,
			"Would ask: Docker isn't installed.",
			"Would stop: Installing Docker needs curl.",
			"Would run: docker pull backproofhq/runner:latest",
			"Would ask: Start the runner container now? [Y/n]",
		)

		code, output = run(t, "", "sh", "-c", `mkdir /fake && printf '#!/bin/sh\n' > /fake/curl && chmod +x /fake/curl && PATH=/fake:$PATH sh /install.sh --token bp_inst_test --dry-run`)
		if code != 0 {
			t.Errorf("with curl: exit code = %d, want 0\n%s", code, output)
		}
		assertContains(t, output, "Would run: sh -c 'curl -fsSL https://get.docker.com | sh'")
	})

	t.Run("bad options are refused", func(t *testing.T) {
		for _, args := range [][]string{{"--drain-timeout", "soon"}, {"--version", "latest"}, {"--colour"}, {"--token"}} {
			code, output := run(t, "", append([]string{"sh", "/install.sh", "--dry-run"}, args...)...)
			if code != 2 || !strings.Contains(output, "Options:") {
				t.Errorf("%v: exit code = %d, want 2 with the usage\n%s", args, code, output)
			}
		}
	})
}
