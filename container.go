package docker

import (
	"context"
	"fmt"
	"io/ioutil"
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
	zen_targets.BaseFields `mapstructure:",squash"`
	ContainerName          string            `mapstructure:"container"`
	Image                  string            `mapstructure:"image"`
	EnvFiles               []string          `mapstructure:"env_files"`
	Command                string            `mapstructure:"command"`
	Entrypoint             string            `mapstructure:"entrypoint"`
	Daemon                 bool              `mapstructure:"daemon"`
	Volumes                map[string]string `mapstructure:"volumes"`
	Ports                  map[string]string `mapstructure:"ports"`
}

func (dcc DockerContainerConfig) GetTargets(_ *zen_targets.TargetConfigContext) ([]*zen_targets.Target, error) {
	opts := []zen_targets.TargetOption{
		zen_targets.WithSrcs(map[string][]string{"_envs": dcc.EnvFiles}),
		zen_targets.WithOuts(dcc.EnvFiles),
		zen_targets.WithEnvVars(dcc.Env),
		zen_targets.WithSecretEnvVars(dcc.SecretEnv),
		zen_targets.WithLabels(dcc.Labels),
		zen_targets.WithTargetScript("deploy", &zen_targets.TargetScript{
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
					cmd = append(cmd, "-v", fmt.Sprintf("%s=%s", k, v))
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

				// out, err := cli.ImagePull(ctx, dcc.Image, types.ImagePullOptions{})
				// if err != nil {
				// 	target.Info(err.Error())
				// 	return fmt.Errorf("pulling image: %w", err)
				// }

				// io.Copy(ioutil.Discard, out)
				// out.Close()

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
				}

				for k, v := range dcc.Volumes {
					hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
						Type:   mount.TypeBind,
						Source: k,
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

				statusCh, errCh := cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
				select {
				case err := <-errCh:
					if err != nil {
						return fmt.Errorf("waiting for container to start: %w", err)
					}
				case <-statusCh:
				}

				// out, err := cli.ContainerLogs(ctx, resp.ID, types.ContainerLogsOptions{ShowStdout: true})
				// if err != nil {
				// 	panic(err)
				// }
				return nil
			},
		}),
	}

	return []*zen_targets.Target{
		zen_targets.NewTarget(
			dcc.Name,
			opts...,
		),
	}, nil
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
	env := target.GetEnvironmentVariablesList()

	for _, f := range target.Srcs["_envs"] {
		content, err := ioutil.ReadFile(f)
		if err != nil {
			return nil, err
		}

		env = append(env, strings.Split(string(content), "\n")...)
	}

	return env, nil
}
