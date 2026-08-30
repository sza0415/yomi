package memory

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	defaultQdrantContainer = "yomi-qdrant"
	defaultQdrantImage     = "qdrant/qdrant:latest"
	defaultQdrantVolume    = "yomi-qdrant-data"
)

// EnsureLocalQdrant starts or reuses the development Qdrant container when
// endpoint points at localhost. It preserves the historical behavior of not
// stopping the container; callers that own the process lifecycle should use
// EnsureLocalQdrantManaged instead.
func EnsureLocalQdrant(ctx context.Context, endpoint string) error {
	_, err := EnsureLocalQdrantManaged(ctx, endpoint)
	return err
}

// EnsureLocalQdrantManaged starts or reuses the development Qdrant container
// and returns a cleanup function. The cleanup function stops the container
// only when this call started an existing stopped container or created a new
// one. A container that was already running is left to its external owner.
func EnsureLocalQdrantManaged(ctx context.Context, endpoint string) (func(context.Context) error, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("memory: invalid qdrant endpoint %q", endpoint)
	}
	if !isLocalHost(u.Hostname()) {
		return nil, fmt.Errorf("memory: refusing to manage non-local qdrant endpoint %q", endpoint)
	}
	port := u.Port()
	if port == "" {
		port = "6333"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("memory: invalid qdrant port %q", port)
	}

	managed := false
	inspect := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", defaultQdrantContainer)
	out, inspectErr := inspect.Output()
	if inspectErr == nil {
		if strings.TrimSpace(string(out)) != "true" {
			if err := runDocker(ctx, "start", defaultQdrantContainer); err != nil {
				return nil, fmt.Errorf("memory: start qdrant container: %w", err)
			}
			managed = true
		}
	} else {
		var exitErr *exec.ExitError
		if !errors.As(inspectErr, &exitErr) {
			return nil, fmt.Errorf("memory: run docker inspect: %w", inspectErr)
		}
		if err := ensureDockerImage(ctx, defaultQdrantImage); err != nil {
			return nil, err
		}
		if err := runDocker(ctx, "run", "-d", "--name", defaultQdrantContainer, "-p", port+":6333", "-v", defaultQdrantVolume+":/qdrant/storage", defaultQdrantImage); err != nil {
			return nil, fmt.Errorf("memory: create qdrant container: %w", err)
		}
		managed = true
	}
	if err := waitForQdrant(ctx, strings.TrimRight(endpoint, "/")); err != nil {
		if managed {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = runDocker(stopCtx, "stop", defaultQdrantContainer)
			cancel()
		}
		return nil, err
	}
	if !managed {
		return func(context.Context) error { return nil }, nil
	}
	return func(stopCtx context.Context) error {
		if stopCtx == nil {
			stopCtx = context.Background()
		}
		if err := runDocker(stopCtx, "stop", defaultQdrantContainer); err != nil {
			return fmt.Errorf("memory: stop qdrant container: %w", err)
		}
		return nil
	}, nil
}

func ensureDockerImage(ctx context.Context, image string) error {
	inspect := exec.CommandContext(ctx, "docker", "image", "inspect", image)
	if output, err := inspect.CombinedOutput(); err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("memory: qdrant image %q is not available locally; pull it manually first: %s", image, message)
	}
	return nil
}

func runDocker(ctx context.Context, args ...string) error {
	command := exec.CommandContext(ctx, "docker", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}

func waitForQdrant(ctx context.Context, endpoint string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/collections", nil)
		if err == nil {
			resp, requestErr := client.Do(req)
			if requestErr == nil {
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("memory: qdrant did not become ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func isLocalHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
