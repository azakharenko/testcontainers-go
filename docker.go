package testcontainers

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/cenkalti/backoff"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"

	"github.com/testcontainers/testcontainers-go/wait"
)

// Implement interfaces
var _ Container = (*DockerContainer)(nil)

// DockerContainer represents a container started using Docker
type DockerContainer struct {
	// Container ID from Docker
	ID         string
	WaitingFor wait.Strategy

	// Cache to retrieve container infromation without re-fetching them from dockerd
	raw               *types.ContainerJSON
	provider          *DockerProvider
	sessionID         uuid.UUID
	terminationSignal chan bool
	skipReaper        bool
}

func (c *DockerContainer) GetContainerID() string {
	return c.ID
}

// Endpoint gets proto://host:port string for the first exposed port
// Will returns just host:port if proto is ""
func (c *DockerContainer) Endpoint(ctx context.Context, proto string) (string, error) {
	ports, err := c.Ports(ctx)
	if err != nil {
		return "", err
	}

	// get first port
	var firstPort nat.Port
	for p := range ports {
		firstPort = p
		break
	}

	return c.PortEndpoint(ctx, firstPort, proto)
}

// PortEndpoint gets proto://host:port string for the given exposed port
// Will returns just host:port if proto is ""
func (c *DockerContainer) PortEndpoint(ctx context.Context, port nat.Port, proto string) (string, error) {
	host, err := c.Host(ctx)
	if err != nil {
		return "", err
	}

	outerPort, err := c.MappedPort(ctx, port)
	if err != nil {
		return "", err
	}

	protoFull := ""
	if proto != "" {
		protoFull = fmt.Sprintf("%s://", proto)
	}

	return fmt.Sprintf("%s%s:%s", protoFull, host, outerPort.Port()), nil
}

// Host gets host (ip or name) of the docker daemon where the container port is exposed
// Warning: this is based on your Docker host setting. Will fail if using an SSH tunnel
// You can use the "TC_HOST" env variable to set this yourself
func (c *DockerContainer) Host(ctx context.Context) (string, error) {
	host, err := c.provider.daemonHost()
	if err != nil {
		return "", err
	}

	return host, nil
}

// MappedPort gets externally mapped port for a container port
func (c *DockerContainer) MappedPort(ctx context.Context, port nat.Port) (nat.Port, error) {
	ports, err := c.Ports(ctx)
	if err != nil {
		return "", err
	}

	for k, p := range ports {
		if k.Port() != port.Port() {
			continue
		}
		if port.Proto() != "" && k.Proto() != port.Proto() {
			continue
		}
		return nat.NewPort(k.Proto(), p[0].HostPort)
	}

	return "", errors.New("port not found")
}

// Ports gets the exposed ports for the container.
func (c *DockerContainer) Ports(ctx context.Context) (nat.PortMap, error) {
	inspect, err := c.inspectContainer(ctx)
	if err != nil {
		return nil, err
	}

	return inspect.NetworkSettings.Ports, nil
}

// SessionID gets the current session id
func (c *DockerContainer) SessionID() string {
	return c.sessionID.String()
}

// IsRunning returns true if a container is running
func (c *DockerContainer) IsRunning(ctx context.Context) (bool, error) {
	state, err := c.State(ctx)
	if err != nil {
		return false, err
	}

	return state.Running, nil
}

// State returns current container's state
func (c *DockerContainer) State(ctx context.Context) (*types.ContainerState, error) {
	c.ResetCache(ctx)
	inspect, err := c.inspectContainer(ctx)
	if err != nil {
		return nil, err
	}

	return inspect.State, nil
}

// State returns a container's image
func (c *DockerContainer) Image(ctx context.Context) (string, error) {
	inspect, err := c.inspectContainer(ctx)
	if err != nil {
		return "", err
	}

	return inspect.Image, nil
}

// Start will start an already created container
func (c *DockerContainer) Start(ctx context.Context) error {
	if err := c.provider.client.ContainerStart(ctx, c.ID, types.ContainerStartOptions{}); err != nil {
		return err
	}

	// if a Wait Strategy has been specified, wait before returning
	if c.WaitingFor != nil {
		if err := c.WaitingFor.WaitUntilReady(ctx, c); err != nil {
			return err
		}
	}

	return nil
}

// Stop will stop a container
func (c *DockerContainer) Stop(ctx context.Context) error {
	if err := c.provider.client.ContainerStop(ctx, c.ID, nil); err != nil {
		return fmt.Errorf("could not stop container '%s': %s", c.ID, err)
	}

	return nil
}

// Remove will remove a container
func (c *DockerContainer) Remove(ctx context.Context, force bool) error {
	removeOpts := types.ContainerRemoveOptions{
		RemoveVolumes: false,
		RemoveLinks:   false,
		Force:         force,
	}
	if err := c.provider.client.ContainerRemove(ctx, c.ID, removeOpts); err != nil {
		return fmt.Errorf("could not remove container '%s': %s", c.ID, err)
	}

	return nil
}

// Terminate is used to kill the container. It is usally triggered by as defer function.
func (c *DockerContainer) Terminate(ctx context.Context) error {
	err := c.provider.client.ContainerRemove(ctx, c.GetContainerID(), types.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	})

	return err
}

func (c *DockerContainer) inspectContainer(ctx context.Context) (*types.ContainerJSON, error) {
	if c.raw != nil {
		return c.raw, nil
	}

	inspect, err := c.provider.client.ContainerInspect(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	c.raw = &inspect

	return c.raw, nil
}

// Logs will fetch both STDOUT and STDERR from the current container. Returns a
// ReadCloser and leaves it up to the caller to extract what it wants.
func (c *DockerContainer) Logs(ctx context.Context) (io.ReadCloser, error) {
	options := types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	}

	return c.provider.client.ContainerLogs(ctx, c.ID, options)
}

// Name gets the name of the container.
func (c *DockerContainer) Name(ctx context.Context) (string, error) {
	inspect, err := c.inspectContainer(ctx)
	if err != nil {
		return "", err
	}

	return inspect.Name, nil
}

// ResetCache sets struct field raw to nil
func (c *DockerContainer) ResetCache(ctx context.Context) {
	c.raw = nil
}

// DockerProvider implements the ContainerProvider interface
type DockerProvider struct {
	client    *client.Client
	hostCache string
}

var _ ContainerProvider = (*DockerProvider)(nil)

// NewDockerProvider creates a Docker provider with the EnvClient
func NewDockerProvider() (*DockerProvider, error) {
	client, err := client.NewEnvClient()
	if err != nil {
		return nil, err
	}

	client.NegotiateAPIVersion(context.Background())
	p := &DockerProvider{
		client: client,
	}

	return p, nil
}

// CreateContainer fulfills a request for a container without starting it
func (p *DockerProvider) CreateContainer(ctx context.Context, req ContainerRequest) (Container, error) {
	exposedPortSet, exposedPortMap, err := nat.ParsePortSpecs(req.ExposedPorts)
	if err != nil {
		return nil, err
	}

	env := []string{}
	for envKey, envVar := range req.Env {
		env = append(env, envKey+"="+envVar)
	}

	if req.Labels == nil {
		req.Labels = make(map[string]string)
	}

	sessionID := uuid.NewV4()

	var termSignal chan bool
	if !req.SkipReaper {
		r, err := NewReaper(ctx, sessionID.String(), p)
		if err != nil {
			return nil, errors.Wrap(err, "creating reaper failed")
		}
		termSignal, err = r.Connect()
		if err != nil {
			return nil, errors.Wrap(err, "connecting to reaper failed")
		}
		for k, v := range r.Labels() {
			if _, ok := req.Labels[k]; !ok {
				req.Labels[k] = v
			}
		}
	}

	dockerInput := &container.Config{
		Image:        req.Image,
		Env:          env,
		ExposedPorts: exposedPortSet,
		Labels:       req.Labels,
	}

	if req.Cmd != "" {
		dockerInput.Cmd = strings.Split(req.Cmd, " ")
	}

	if len(req.Entrypoint) > 0 {
		dockerInput.Entrypoint = req.Entrypoint
	}

	_, _, err = p.client.ImageInspectWithRaw(ctx, req.Image)
	if err != nil {
		if client.IsErrNotFound(err) {
			pullOpt := types.ImagePullOptions{}
			if req.RegistryCred != "" {
				pullOpt.RegistryAuth = req.RegistryCred
			}
			var pull io.ReadCloser
			err := backoff.Retry(func() error {
				var err error
				pull, err = p.client.ImagePull(ctx, req.Image, pullOpt)
				return err
			}, backoff.NewExponentialBackOff())
			if err != nil {
				return nil, err
			}
			defer pull.Close()

			// download of docker image finishes at EOF of the pull request
			_, err = ioutil.ReadAll(pull)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	// prepare mounts
	bindMounts := []mount.Mount{}
	for hostPath, innerPath := range req.BindMounts {
		bindMounts = append(bindMounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: hostPath,
			Target: innerPath,
		})
	}

	hostConfig := &container.HostConfig{
		PortBindings: exposedPortMap,
		Mounts:       bindMounts,
		AutoRemove:   !req.DontRemove,
		Privileged:   req.Privileged,
	}

	resp, err := p.client.ContainerCreate(ctx, dockerInput, hostConfig, nil, req.Name)
	if err != nil {
		return nil, err
	}

	c := &DockerContainer{
		ID:                resp.ID,
		WaitingFor:        req.WaitingFor,
		sessionID:         sessionID,
		provider:          p,
		terminationSignal: termSignal,
		skipReaper:        req.SkipReaper,
	}

	return c, nil
}

// ListContainers returns current existent containers
func (p *DockerProvider) ListContainers(ctx context.Context, all bool) ([]Container, error) {
	containers, err := p.client.ContainerList(ctx, types.ContainerListOptions{All: all})
	if err != nil {
		return nil, fmt.Errorf("error while trying to list containers: %s", err)
	}

	result := make([]Container, 0, len(containers))
	for _, c := range containers {
		result = append(result, &DockerContainer{ID: c.ID, provider: p})
	}

	return result, nil
}

// ContainerExists returns true if container with given name exists
func (p *DockerProvider) ContainerExists(ctx context.Context, name string) (bool, error) {
	containers, err := p.ListContainers(ctx, true)
	if err != nil {
		return false, err
	}

	for _, c := range containers {
		currentName, err := c.Name(ctx)
		if err != nil {
			return false, fmt.Errorf("error while trying to get container's name: %s", err)
		}
		if currentName == "/"+name {
			return true, nil
		}
	}

	return false, nil
}

// CreateFromExistentContainer returns Container interface that uses existent container
func (p *DockerProvider) CreateFromExistentContainer(ctx context.Context, containerName string) (Container, error) {
	sessionID := uuid.NewV4()

	inspect, err := p.client.ContainerInspect(ctx, containerName) // we can use name instead of ID
	if err != nil {
		return nil, fmt.Errorf("error while trying to inspect thew container: %s", err)
	}

	c := &DockerContainer{
		ID:        inspect.ID,
		sessionID: sessionID,
		provider:  p,
	}

	return c, nil
}

// RunContainer takes a RequestContainer as input and it runs a container via the docker sdk
func (p *DockerProvider) RunContainer(ctx context.Context, req ContainerRequest) (Container, error) {
	c, err := p.CreateContainer(ctx, req)
	if err != nil {
		return nil, err
	}

	if err := c.Start(ctx); err != nil {
		return c, errors.Wrap(err, "could not start container")
	}

	return c, nil
}

// daemonHost gets the host or ip of the Docker daemon where ports are exposed on
// Warning: this is based on your Docker host setting. Will fail if using an SSH tunnel
// You can use the "TC_HOST" env variable to set this yourself
func (p *DockerProvider) daemonHost() (string, error) {
	if p.hostCache != "" {
		return p.hostCache, nil
	}

	host, exists := os.LookupEnv("TC_HOST")
	if exists {
		p.hostCache = host
		return p.hostCache, nil
	}

	// infer from Docker host
	url, err := url.Parse(p.client.DaemonHost())
	if err != nil {
		return "", err
	}

	switch url.Scheme {
	case "http", "https", "tcp":
		p.hostCache = url.Hostname()
	case "unix", "npipe":
		if inAContainer() {
			ip, err := getGatewayIp()
			if err != nil {
				return "", err
			}
			p.hostCache = ip
		} else {
			p.hostCache = "localhost"
		}
	default:
		return "", errors.New("Could not determine host through env or docker host")
	}

	return p.hostCache, nil
}

func inAContainer() bool {
	// see https://github.com/testcontainers/testcontainers-java/blob/3ad8d80e2484864e554744a4800a81f6b7982168/core/src/main/java/org/testcontainers/dockerclient/DockerClientConfigUtils.java#L15
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

func getGatewayIp() (string, error) {
	// see https://github.com/testcontainers/testcontainers-java/blob/3ad8d80e2484864e554744a4800a81f6b7982168/core/src/main/java/org/testcontainers/dockerclient/DockerClientConfigUtils.java#L27
	cmd := exec.Command("sh", "-c", "ip route|awk '/default/ { print $3 }'")
	stdout, err := cmd.Output()
	if err != nil {
		return "", errors.New("Failed to detect docker host")
	}
	ip := strings.TrimSpace(string(stdout))
	if len(ip) == 0 {
		return "", errors.New("Failed to parse default gateway IP")
	}
	return string(ip), nil
}
