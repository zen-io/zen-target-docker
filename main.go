package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	zen_targets "github.com/zen-io/zen-core/target"
)

var KnownTargets = zen_targets.TargetCreatorMap{
	"docker_container": DockerContainerConfig{},
	"docker_image":     DockerImageConfig{},
}

type dockerStreamer struct {
	out io.Writer
	err func(msg string)
}

func (ds *dockerStreamer) Write(b []byte) (n int, err error) {
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var data map[string]any
		if err := json.Unmarshal(line, &data); err != nil {
			return 0, fmt.Errorf("unmarshalling docker output: %w", err)
		}

		if val, ok := data["stream"]; ok {
			ds.out.Write([]byte(val.(string)))
		} else if val, ok := data["error"]; ok {
			return len(b), fmt.Errorf("building error: %s", val.(string))
		}
	}

	return len(b), nil
}
