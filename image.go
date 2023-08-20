package docker

import (
	"fmt"
	"io"
	"path/filepath"

	environs "github.com/zen-io/zen-core/environments"
	zen_targets "github.com/zen-io/zen-core/target"
	"github.com/zen-io/zen-core/utils"

	ecr "github.com/awslabs/amazon-ecr-credential-helper/ecr-login"
	"github.com/chrismellard/docker-credential-acr-env/pkg/credhelper"
	"github.com/google/go-containerregistry/pkg/authn"
)

var (
	amazonKeychain authn.Keychain = authn.NewKeychainFromHelper(ecr.NewECRHelper(ecr.WithLogger(io.Discard)))
	azureKeychain  authn.Keychain = authn.NewKeychainFromHelper(credhelper.NewACRCredentialsHelper())
)

type DockerImageConfig struct {
	Srcs          []string                         `mapstructure:"srcs"`
	BuildArgs     map[string]string                `mapstructure:"build_args"`
	Dockerfile    *string                          `mapstructure:"dockerfile"`
	DockerIgnore  *string                          `mapstructure:"dockerignore"`
	Image         string                           `mapstructure:"image"`
	Context       *string                          `mapstructure:"context"`
	Registry      *string                          `mapstructure:"registry"`
	Tags          []string                         `mapstructure:"tags"`
	Platform      *string                          `mapstructure:"platform"`
	DeployDeps    []string                         `mapstructure:"deploy_deps"`
	Daemon        bool                             `mapstructure:"daemon"`
	Buildx        *string                          `mapstructure:"buildx_toolchain"`
	Crane         *string                          `mapstructure:"crane_toolchain"`
	Name          string                           `mapstructure:"name" zen:"yes" desc:"Name for the target"`
	Description   string                           `mapstructure:"desc" zen:"yes" desc:"Target description"`
	Labels        []string                         `mapstructure:"labels" zen:"yes" desc:"Labels to apply to the targets"` //
	Deps          []string                         `mapstructure:"deps" zen:"yes" desc:"Build dependencies"`
	PassEnv       []string                         `mapstructure:"pass_env" zen:"yes" desc:"List of environment variable names that will be passed from the OS environment, they are part of the target hash"`
	PassSecretEnv []string                         `mapstructure:"pass_secret_env" zen:"yes" desc:"List of environment variable names that will be passed from the OS environment, they are not used to calculate the target hash"`
	Env           map[string]string                `mapstructure:"env" zen:"yes" desc:"Key-Value map of static environment variables to be used"`
	Visibility    []string                         `mapstructure:"visibility" zen:"yes" desc:"List of visibility for this target"`
	Environments  map[string]*environs.Environment `mapstructure:"environments" zen:"yes" desc:"Deployment Environments"`
}

func (dic DockerImageConfig) GetTargets(tcc *zen_targets.TargetConfigContext) ([]*zen_targets.TargetBuilder, error) {
	if dic.Dockerfile == nil {
		dic.Dockerfile = utils.StringPtr("Dockerfile")
	}
	if dic.Platform == nil {
		dic.Platform = utils.StringPtr("linux/amd64")
	}

	toolchains := map[string]string{}
	if dic.Buildx != nil {
		toolchains["buildx"] = *dic.Buildx
	} else if val, ok := tcc.KnownToolchains["buildx"]; !ok {
		return nil, fmt.Errorf("buildx toolchain is not configured")
	} else {
		toolchains["buildx"] = val
	}

	if dic.Crane != nil {
		toolchains["crane"] = *dic.Crane
	} else {
		if val, ok := tcc.KnownToolchains["crane"]; !ok {
			return nil, fmt.Errorf("crane toolchain is not configured")
		} else {
			toolchains["crane"] = val
		}
	}

	if len(dic.Tags) == 0 {
		dic.Tags = []string{"latest"}
	}

	t := zen_targets.ToTarget(dic)
	t.Srcs = map[string][]string{"context": dic.Srcs, "dockerfile": {*dic.Dockerfile}}
	t.Outs = []string{"image.tar"}

	t.Scripts["build"] = &zen_targets.TargetBuilderScript{
		Deps: dic.Deps,
		Run: func(target *zen_targets.Target, runCtx *zen_targets.RuntimeContext) error {
			target.SetStatus("Building image %s:%s", dic.Image, dic.Tags[0])

			var context string
			if dic.Context != nil {
				context = filepath.Join(target.Cwd, *dic.Context)
			} else {
				context = target.Cwd
			}

			args := []string{
				target.Tools["buildx"], "build", context,
				"--output", fmt.Sprintf("type=docker,dest=%s/image.tar", target.Cwd),
				"--file", target.Srcs["dockerfile"][0],
			}

			interpolBuildArgs, err := utils.InterpolateMap(dic.BuildArgs, target.Env)
			if err != nil {
				return fmt.Errorf("interpolating build args: %w", err)
			}
			for k, v := range interpolBuildArgs {
				args = append(args, "--build-arg", fmt.Sprintf("%s=%s", k, v))
			}

			return target.Exec(args, "docker build")
		},
	}

	t.Scripts["deploy"] = &zen_targets.TargetBuilderScript{
		Alias: []string{"push"},
		Deps:  dic.DeployDeps,
		Run: func(target *zen_targets.Target, runCtx *zen_targets.RuntimeContext) error {
			target.SetStatus("Pushing image %s:%s", dic.Image, dic.Tags[0])

			if dic.Registry == nil {
				if val, ok := target.Env["DOCKER_REGISTRY"]; !ok {
					return fmt.Errorf("need to provide a docker registry or a default via DOCKER_REGISTRY env")
				} else {
					dic.Registry = utils.StringPtr(val)
				}
			}

			tags := []string{}
			for _, t := range dic.Tags {
				tags = append(tags, fmt.Sprintf("%s/%s:%s", *dic.Registry, dic.Image, t))
			}
			kraneCmd := []string{target.Tools["crane"], "push", filepath.Join(target.Cwd, "image.tar"), tags[0]}

			if err := target.Exec(kraneCmd, "pushing image"); err != nil {
				return err
			}

			for _, t := range tags[1:] {
				tagCmd := []string{target.Tools["crane"], "tag", tags[0], t}
				if err := target.Exec(tagCmd, "tagging image"); err != nil {
					return err
				}
			}

			target.SetStatus("Pushed %s:%s", dic.Image, dic.Tags[0])
			return nil
		},
	}

	t.Scripts["load"] = &zen_targets.TargetBuilderScript{
		Alias: []string{"push"},
		Run: func(target *zen_targets.Target, runCtx *zen_targets.RuntimeContext) error {
			target.SetStatus("Loading image %s:%s to docker", dic.Image, dic.Tags[0])

			tags := []string{}
			for _, t := range dic.Tags {
				tags = append(tags, fmt.Sprintf("%s/%s:%s", *dic.Registry, dic.Image, t))
			}

			loadCmd := []string{target.Tools["buildx"], "load", filepath.Join(target.Cwd, "image.tar"), tags[0]}
			if err := target.Exec(loadCmd, "loading image"); err != nil {
				return err
			}

			target.SetStatus("Loaded %s:%s", dic.Image, dic.Tags[0])
			return nil
		},
	}

	return []*zen_targets.TargetBuilder{t}, nil
}
