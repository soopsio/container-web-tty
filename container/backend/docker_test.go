package backend

import (
	"context"
	"testing"

	"github.com/wrfly/container-web-tty/config"
)

func TestDocker(t *testing.T) {
	ctx := context.Background()
	dockerConf := config.DockerConfig{
		DockerSock: "/var/run/docker.sock",
	}
	t.Run("test new docker client", func(t *testing.T) {
		cli, err := newDockerCli(dockerConf)
		if err != nil {
			t.Error(err)
		}
		for _, c := range cli.List(ctx) {
			t.Logf("got container: %s", c.ID)
		}
	})
}
