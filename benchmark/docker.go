package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// BuildImage builds a Docker image from the given solution directory.
func BuildImage(ctx context.Context, cli *client.Client, solutionDir, tag string) error {
	tarBuf, err := createTarball(solutionDir)
	if err != nil {
		return fmt.Errorf("create tarball: %w", err)
	}

	opts := types.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: "Dockerfile",
		Remove:     true,
	}

	resp, err := cli.ImageBuild(ctx, tarBuf, opts)
	if err != nil {
		return fmt.Errorf("image build: %w", err)
	}
	defer resp.Body.Close()

	// Drain output; surface any build errors with full context.
	dec := json.NewDecoder(resp.Body)
	var buildLog strings.Builder
	for {
		var msg struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("reading build output: %w", err)
		}
		if msg.Stream != "" {
			buildLog.WriteString(msg.Stream)
		}
		if msg.Error != "" {
			return fmt.Errorf("docker build error: %s\nbuild log:\n%s", msg.Error, buildLog.String())
		}
	}
	return nil
}

// RunContainer creates and starts a container, returning its ID and the assigned host port.
func RunContainer(ctx context.Context, cli *client.Client, tag string, containerPort int) (string, int, error) {
	portSpec := fmt.Sprintf("%d/tcp", containerPort)
	natPort := nat.Port(portSpec)

	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: tag,
			ExposedPorts: nat.PortSet{
				natPort: struct{}{},
			},
		},
		&container.HostConfig{
			PortBindings: nat.PortMap{
				natPort: []nat.PortBinding{
					{HostIP: "127.0.0.1", HostPort: ""}, // let Docker pick
				},
			},
		},
		nil, nil, "",
	)
	if err != nil {
		return "", 0, fmt.Errorf("container create: %w", err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", 0, fmt.Errorf("container start: %w", err)
	}

	// Read the port Docker actually assigned.
	// NetworkSettings.Ports may be transiently empty right after ContainerStart
	// on some Docker Desktop setups, so retry briefly.
	deadline := time.Now().Add(5 * time.Second)
	for {
		inspect, err := cli.ContainerInspect(ctx, resp.ID)
		if err != nil {
			return "", 0, fmt.Errorf("container inspect: %w", err)
		}
		bindings := inspect.NetworkSettings.Ports[natPort]
		if len(bindings) > 0 {
			hostPort, err := strconv.Atoi(bindings[0].HostPort)
			if err != nil {
				return "", 0, fmt.Errorf("parse host port %q: %w", bindings[0].HostPort, err)
			}
			return resp.ID, hostPort, nil
		}
		if time.Now().After(deadline) {
			return "", 0, fmt.Errorf("no port binding found for %s after 5s", portSpec)
		}
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// WaitHealthy polls GET /healthz until the service returns 200, using exponential backoff.
func WaitHealthy(ctx context.Context, baseURL string, timeoutMs int) error {
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	url := baseURL + "/healthz"
	client := &http.Client{Timeout: 2 * time.Second}

	delay := 100 * time.Millisecond
	const maxDelay = 2 * time.Second

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("service at %s not healthy within %dms", baseURL, timeoutMs)
		}

		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}

		delay = time.Duration(math.Min(float64(delay*2), float64(maxDelay)))
	}
}

// StopContainer stops and removes the given container.
func StopContainer(ctx context.Context, cli *client.Client, containerID string) error {
	timeout := 10
	if err := cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("container stop: %w", err)
	}
	if err := cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("container remove: %w", err)
	}
	return nil
}

// ImageTag returns the Docker image tag for a solution name.
func ImageTag(name string) string {
	slug := strings.ToLower(name)
	slug = strings.ReplaceAll(slug, " ", "-")
	return "dl-benchmark-" + slug
}

// ContainerName returns a stable Docker container name for a solution.
func ContainerName(name string) string {
	slug := strings.ToLower(name)
	slug = strings.ReplaceAll(slug, " ", "-")
	return "dl-bench-" + slug
}

// CreateBenchmarkNetwork creates an isolated bridge network for a benchmark run.
func CreateBenchmarkNetwork(ctx context.Context, cli *client.Client) (networkID, networkName string, err error) {
	name := fmt.Sprintf("dl-benchmark-%d", time.Now().UnixMilli())
	resp, createErr := cli.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge"})
	if createErr != nil {
		return "", "", fmt.Errorf("network create: %w", createErr)
	}
	return resp.ID, name, nil
}

// RemoveNetwork removes the Docker network with the given ID.
func RemoveNetwork(ctx context.Context, cli *client.Client, networkID string) error {
	if err := cli.NetworkRemove(ctx, networkID); err != nil {
		return fmt.Errorf("network remove: %w", err)
	}
	return nil
}

// ConnectSelfToNetwork connects the runner's own container to the given network.
// Docker sets HOSTNAME to the short container ID, which is used to identify this container.
func ConnectSelfToNetwork(ctx context.Context, cli *client.Client, networkID string) error {
	hostname := os.Getenv("HOSTNAME")
	if hostname == "" {
		return fmt.Errorf("HOSTNAME env var not set")
	}
	if err := cli.NetworkConnect(ctx, networkID, hostname, nil); err != nil {
		return fmt.Errorf("network connect self (%s): %w", hostname, err)
	}
	return nil
}

// RunContainerOnNetwork creates and starts a container on the given Docker network.
// No host port binding is used; the container is reachable by name within the network.
func RunContainerOnNetwork(ctx context.Context, cli *client.Client, tag string, containerPort int, networkName, containerName string) (string, error) {
	portSpec := fmt.Sprintf("%d/tcp", containerPort)
	natPort := nat.Port(portSpec)

	// Remove any leftover container with this name (e.g. from a previous aborted run).
	_ = cli.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true})

	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:        tag,
			ExposedPorts: nat.PortSet{natPort: struct{}{}},
		},
		&container.HostConfig{},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				networkName: {Aliases: []string{containerName}},
			},
		},
		nil,
		containerName,
	)
	if err != nil {
		return "", fmt.Errorf("container create: %w", err)
	}
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("container start: %w", err)
	}
	return resp.ID, nil
}

// createTarball creates an in-memory tar archive of the given directory.
func createTarball(dir string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		// Use forward slashes in tar headers (required by Docker).
		rel = filepath.ToSlash(rel)

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}
