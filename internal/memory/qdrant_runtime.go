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
// endpoint points at localhost. It never removes or stops the container.
func EnsureLocalQdrant(ctx context.Context, endpoint string) error {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("memory: invalid qdrant endpoint %q", endpoint)
	}
	if !isLocalHost(u.Hostname()) {
		return fmt.Errorf("memory: refusing to manage non-local qdrant endpoint %q", endpoint)
	}
	port := u.Port()
	if port == "" {
		port = "6333"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("memory: invalid qdrant port %q", port)
	}

	inspect := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", defaultQdrantContainer)
	out, inspectErr := inspect.Output()
	if inspectErr == nil {
		if strings.TrimSpace(string(out)) != "true" {
			if err := runDocker(ctx, "start", defaultQdrantContainer); err != nil {
				return fmt.Errorf("memory: start qdrant container: %w", err)
			}
		}
	} else {
		var exitErr *exec.ExitError
		if !errors.As(inspectErr, &exitErr) {
			return fmt.Errorf("memory: run docker inspect: %w", inspectErr)
		}
		if err := ensureDockerImage(ctx, defaultQdrantImage); err != nil {
			return err
		}
		if err := runDocker(ctx, "run", "-d", "--name", defaultQdrantContainer, "-p", port+":6333", "-v", defaultQdrantVolume+":/qdrant/storage", defaultQdrantImage); err != nil {
			return fmt.Errorf("memory: create qdrant container: %w", err)
		}
	}
	return waitForQdrant(ctx, strings.TrimRight(endpoint, "/"))
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
