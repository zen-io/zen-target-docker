package docker

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"path/filepath"
	"strconv"
	"strings"

	zen_targets "github.com/zen-io/zen-core/target"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

type DockerContainerConfig struct {
	Name          string            `mapstructure:"name" zen:"yes" desc:"Name for the target"`
	Description   string            `mapstructure:"desc" zen:"yes" desc:"Target description"`
	Labels        []string          `mapstructure:"labels" zen:"yes" desc:"Labels to apply to the targets"` //
	Deps          []string          `mapstructure:"deps" zen:"yes" desc:"Build dependencies"`
	PassEnv       []string          `mapstructure:"pass_env" zen:"yes" desc:"List of environment variable names that will be passed from the OS environment, they are part of the target hash"`
	PassSecretEnv []string          `mapstructure:"secret_env" zen:"yes" desc:"List of environment variable names that will be passed from the OS environment, they are not used to calculate the target hash"`
	Env           map[string]string `mapstructure:"env" zen:"yes" desc:"Key-Value map of static environment variables to be used"`
	Visibility    []string          `mapstructure:"visibility" zen:"yes" desc:"List of visibility for this target"`
	Memory        *int              `mapstructure:"memory"`
	Cpu           *int              `mapstructure:"cpu"`
	ContainerName string            `mapstructure:"container"`
	Image         string            `mapstructure:"image"`
	EnvFiles      []string          `mapstructure:"env_files"`
	ContainerEnv  map[string]string `mapstructure:"container_env"`
	Command       string            `mapstructure:"command"`
	Entrypoint    string            `mapstructure:"entrypoint"`
	Daemon        bool              `mapstructure:"daemon"`
	Volumes       map[string]string `mapstructure:"volumes"`
	Ports         map[string]string `mapstructure:"ports"`
}

func (dcc DockerContainerConfig) GetTargets(_ *zen_targets.TargetConfigContext) ([]*zen_targets.TargetBuilder, error) {
	for k, v := range dcc.ContainerEnv {
		dcc.Labels = append(dcc.Labels, fmt.Sprintf("container_env=%s=%s", k, v))
	}
	for _, v := range dcc.EnvFiles {
		dcc.Labels = append(dcc.Labels, fmt.Sprintf("env_file=%s", v))
	}

	t := zen_targets.ToTarget(dcc)
	t.Srcs = map[string][]string{"_envs": dcc.EnvFiles}
	t.Outs = dcc.EnvFiles

	t.Scripts["deploy"] = &zen_targets.TargetBuilderScript{
		Pre: func(target *zen_targets.Target, runCtx *zen_targets.RuntimeContext) error {
			cmd := []string{
				"docker",
				"run",
				"-n", dcc.ContainerName,
			}

			env, err := GetContainerEnv(target, runCtx)
			if err != nil {
				return err
			}
			for _, e := range env {
				cmd = append(cmd, "-e", e)
			}

			for k, v := range dcc.Ports {
				cmd = append(cmd, "-p", fmt.Sprintf("%s=%s", k, v))
			}

			for k, v := range dcc.Volumes {
				interpolatedVolume, err := target.Interpolate(k)
				if err != nil {
					return fmt.Errorf("interpolating volume %s: %w", k, err)
				}
				cmd = append(cmd, "-v", fmt.Sprintf("%s=%s", interpolatedVolume, v))
			}

			if dcc.Entrypoint != "" {
				cmd = append(cmd, "--entrypoint", dcc.Entrypoint)
			}
			cmd = append(cmd, dcc.Image)
			if dcc.Command != "" {
				cmd = append(cmd, dcc.Command)
			}

			target.Env["ZEN_DEBUG_CMD"] = strings.Join(cmd, " ")

			return nil
		},
		Run: func(target *zen_targets.Target, runCtx *zen_targets.RuntimeContext) error {
			ctx := context.Background()
			cli, err := client.NewClientWithOpts(client.FromEnv)
			if err != nil {
				return fmt.Errorf("creating docker client: %w", err)
			}
			cli.NegotiateAPIVersion(ctx)

			target.SetStatus("Pulling image " + dcc.Image)

			out, err := cli.ImagePull(ctx, dcc.Image, types.ImagePullOptions{})
			if err != nil {
				return fmt.Errorf("pulling image: %w", err)
			}

			io.Copy(ioutil.Discard, out)
			out.Close()

			// Check if the container already exists
			containerName := dcc.ContainerName
			_, err = cli.ContainerInspect(ctx, containerName)
			if err == nil {
				// Container already exists, do nothing
				target.Debugln("Container %s already exists", containerName)
				return nil
			}
			env, err := GetContainerEnv(target, runCtx)
			if err != nil {
				return fmt.Errorf("computing env for container: %w", err)
			}

			// Container doesn't exist, create a new one
			config := &container.Config{
				Image: dcc.Image,
				Env:   env,
			}
			if dcc.Command != "" {
				config.Cmd = strings.Split(dcc.Command, " ")
			}
			if dcc.Entrypoint != "" {
				config.Entrypoint = strings.Split(dcc.Entrypoint, " ")
			}

			ports, err := GetPortBindings(dcc.Ports)
			if err != nil {
				return fmt.Errorf("computing port binds: %w", err)
			}

			hostConfig := &container.HostConfig{
				PortBindings: ports,
				Mounts:       []mount.Mount{},
				Resources:    container.Resources{},
			}

			if dcc.Memory != nil {
				hostConfig.Resources.Memory = (int64(*dcc.Memory)*1000000)			
			}
			if dcc.Cpu != nil {
				hostConfig.Resources.NanoCPUs = (int64(*dcc.Cpu)*1000000000)			
			}

			for k, v := range dcc.Volumes {
				interpolatedVolume, err := target.Interpolate(k)
				if err != nil {
					return fmt.Errorf("interpolating volume %s: %w", k, err)
				}
				hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
					Type:   mount.TypeBind,
					Source: interpolatedVolume,
					Target: v,
				})
			}

			target.SetStatus("creating container " + dcc.Image)
			resp, err := cli.ContainerCreate(ctx, config, hostConfig, nil, nil, containerName)
			if err != nil {
				return fmt.Errorf("creating container: %w", err)
			}

			// Start the container
			target.SetStatus("Starting container " + dcc.Image)
			err = cli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})
			if err != nil {
				return fmt.Errorf("starting container: %w", err)
			}

			target.Debugln("Container %s created and started", containerName)

			return nil
		},
	}

	return []*zen_targets.TargetBuilder{t}, nil
}

func GetPortBindings(ports map[string]string) (m nat.PortMap, err error) {
	m = nat.PortMap{}
	for key, value := range ports {
		if strings.Contains(key, "-") != strings.Contains(value, "-") {
			err = fmt.Errorf("port %s and %s differ in range", key, value)
			return
		}

		var hostPorts, containerPorts, proto, hostIp string

		if strings.Contains(key, "/") {
			spl := strings.Split(key, "/")
			containerPorts = spl[0]
			proto = spl[1]
		} else {
			proto = "tcp"
			containerPorts = key
		}

		if strings.Contains(value, ":") {
			spl := strings.Split(value, ":")
			hostIp = spl[0]
			hostPorts = spl[1]
		} else {
			hostIp = "127.0.0.1"
			hostPorts = value
		}

		hostPortRange := []string{}
		containerPortRange := []string{}
		if strings.Contains(hostPorts, "-") {
			parts := strings.Split(hostPorts, "-")

			start, err := strconv.Atoi(parts[0])
			if err != nil {
				panic("Invalid format")
			}

			end, err := strconv.Atoi(parts[1])
			if err != nil {
				panic("Invalid format")
			}

			for i := start; i <= end; i++ {
				hostPortRange = append(hostPortRange, strconv.Itoa(i))
			}

			parts = strings.Split(containerPorts, "-")

			start, err = strconv.Atoi(parts[0])
			if err != nil {
				panic("Invalid format")
			}

			end, err = strconv.Atoi(parts[1])
			if err != nil {
				panic("Invalid format")
			}

			for i := start; i <= end; i++ {
				containerPortRange = append(containerPortRange, strconv.Itoa(i))
			}
		} else {
			hostPortRange = append(hostPortRange, hostPorts)
			containerPortRange = append(containerPortRange, containerPorts)
		}

		for index := range hostPortRange {
			mappedPort, err := nat.NewPort(proto, containerPortRange[index])
			if err != nil {
				return nil, err
			}
			m[mappedPort] = []nat.PortBinding{{
				HostIP:   hostIp,
				HostPort: hostPortRange[index],
			}}
		}
	}

	return
}

func GetContainerEnv(target *zen_targets.Target, runCtx *zen_targets.RuntimeContext) ([]string, error) {
	env := make([]string, 0)

	for _, label := range target.Labels {
		if strings.HasPrefix(label, "container_env=") {
			interpolatedLabel, err := target.Interpolate(strings.TrimPrefix(label, "container_env="))
			if err != nil {
				return nil, fmt.Errorf("interpolating label %s: %w", label, err)
			}
			env = append(env, interpolatedLabel)
		} else if strings.HasPrefix(label, "env_file=") {
			content, err := ioutil.ReadFile(filepath.Join(target.Cwd, strings.TrimPrefix(label, "env_file=")))
			if err != nil {
				return nil, err
			}

			for _, e := range strings.Split(string(content), "\n") {
				if len(e) == 0 {
					continue
				}
				interpolatedEnv, err := target.Interpolate(e)
				if err != nil {
					return nil, fmt.Errorf("interpolating env %s: %w", e, err)
				}
				env = append(env, interpolatedEnv)
			}
		}

	}

	return env, nil
}
