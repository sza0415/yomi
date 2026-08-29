// Package tools: Docker-based sandbox executor shared by the bash and python tools.
//
// Design: every execution runs in a throwaway `docker run --rm` container so a
// misbehaving command (rm -rf, fork bomb, network abuse) cannot touch the host.
// The workspace is bind-mounted at /work; everything else defaults to locked down.
package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// SandboxConfig controls the isolation applied to each container run.
type SandboxConfig struct {
	Image        string        // container image, e.g. "python:3.12-slim"
	Workspace    string        // host path bind-mounted at /work (resolved, absolute)
	Timeout      time.Duration // wall-clock limit per execution
	MaxOutput    int           // max bytes of combined stdout+stderr returned
	Memory       string        // docker --memory value, e.g. "512m"
	CPUs         string        // docker --cpus value, e.g. "1.0"
	PidsLimit    int           // docker --pids-limit, guards against fork bombs
	TmpSize      string        // /tmp tmpfs size, e.g. "512m"
	Network      bool          // when false, run with --network=none
	DockerBinary string        // docker executable, defaults to "docker"
}

const (
	defaultSandboxTimeout   = 30 * time.Second
	dockerProbeTimeout      = 3 * time.Second
	defaultSandboxMaxOutput = 64 * 1024
	defaultSandboxMemory    = "512m"
	defaultSandboxCPUs      = "1.0"
	defaultSandboxPids      = 256
	defaultSandboxTmpSize   = "64m"
)

// withDefaults returns a copy of cfg with zero fields filled from safe defaults.
func (c SandboxConfig) withDefaults() SandboxConfig {
	if c.Timeout <= 0 {
		c.Timeout = defaultSandboxTimeout
	}
	if c.MaxOutput <= 0 {
		c.MaxOutput = defaultSandboxMaxOutput
	}
	if strings.TrimSpace(c.Memory) == "" {
		c.Memory = defaultSandboxMemory
	}
	if strings.TrimSpace(c.CPUs) == "" {
		c.CPUs = defaultSandboxCPUs
	}
	if c.PidsLimit <= 0 {
		c.PidsLimit = defaultSandboxPids
	}
	if strings.TrimSpace(c.TmpSize) == "" {
		c.TmpSize = defaultSandboxTmpSize
	}
	if strings.TrimSpace(c.DockerBinary) == "" {
		c.DockerBinary = "docker"
	}
	return c
}

// Sandbox runs commands inside disposable Docker containers.
type Sandbox struct {
	cfg SandboxConfig
}

// NewSandbox validates config, the docker binary, and the Docker daemon, then
// returns a reusable sandbox. Image availability is not checked here — the
// first run will surface a clear "pull image" error if it is missing.
func NewSandbox(cfg SandboxConfig) (*Sandbox, error) {
	cfg = cfg.withDefaults()
	if strings.TrimSpace(cfg.Image) == "" {
		return nil, fmt.Errorf("sandbox: image is required")
	}
	if strings.TrimSpace(cfg.Workspace) == "" {
		return nil, fmt.Errorf("sandbox: workspace is required")
	}
	if _, err := exec.LookPath(cfg.DockerBinary); err != nil {
		return nil, fmt.Errorf("sandbox: %q not found in PATH: %w", cfg.DockerBinary, err)
	}
	if err := probeDocker(cfg.DockerBinary); err != nil {
		return nil, err
	}
	return &Sandbox{cfg: cfg}, nil
}

// probeDocker confirms that the CLI can reach a running Docker daemon. A CLI
// binary may be installed while Docker Desktop/the daemon is stopped, so a
// PATH lookup alone is not sufficient to enable execution tools.
func probeDocker(binary string) error {
	ctx, cancel := context.WithTimeout(context.Background(), dockerProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "info", "--format", "{{.ServerVersion}}")
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("sandbox: Docker daemon is not available (docker info timed out after %s)", dockerProbeTimeout)
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("sandbox: Docker daemon is not available: %s", msg)
	}
	return nil
}

// Run executes argv inside a fresh container, piping stdin into it. It returns
// the combined, size-capped output. A timeout or non-zero exit is reported in
// the output string (not as a Go error) so the model can react to it.
func (s *Sandbox) Run(ctx context.Context, argv []string, stdin string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("sandbox: not initialized")
	}
	if len(argv) == 0 {
		return "", fmt.Errorf("sandbox: argv is empty")
	}

	runCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	dockerArgs := s.dockerArgs(argv)
	cmd := exec.CommandContext(runCtx, s.cfg.DockerBinary, dockerArgs...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()

	if runCtx.Err() == context.DeadlineExceeded {
		return s.trim(out.String()) +
			fmt.Sprintf("\n\n[执行超时，已在 %s 后终止。]", s.cfg.Timeout), nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	result := s.trim(out.String())
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if result != "" {
				result += "\n"
			}
			result += fmt.Sprintf("[退出码 %d]", exitErr.ExitCode())
			return result, nil
		}
		// docker itself failed to start (e.g. daemon down, image missing).
		return "", fmt.Errorf("sandbox: docker run failed: %w (output: %s)", err, result)
	}

	if strings.TrimSpace(result) == "" {
		return "（命令执行成功，无输出）", nil
	}
	return result, nil
}

// dockerArgs builds the hardened `docker run` argument list.
func (s *Sandbox) dockerArgs(argv []string) []string {
	args := []string{
		"run", "--rm", "-i",
		"--memory", s.cfg.Memory,
		"--cpus", s.cfg.CPUs,
		"--pids-limit", strconv.Itoa(s.cfg.PidsLimit),
		// Bind the workspace read-write at /work; make everything else immutable.
		"-v", s.cfg.Workspace + ":/work",
		"-w", "/work",
		"--read-only",
		// Writable scratch space that vanishes with the container.
		"--tmpfs", "/tmp:rw,size=" + s.cfg.TmpSize,
	}
	if !s.cfg.Network {
		args = append(args, "--network", "none")
	}
	args = append(args, s.cfg.Image)
	args = append(args, argv...)
	return args
}

// trim caps output to MaxOutput bytes, keeping the head and noting truncation.
func (s *Sandbox) trim(output string) string {
	if len(output) <= s.cfg.MaxOutput {
		return output
	}
	return output[:s.cfg.MaxOutput] +
		fmt.Sprintf("\n\n[输出已截断，最多返回 %d 字节。]", s.cfg.MaxOutput)
}
