package docker

import (
	"github.com/moby/moby/client"
)

var (
	Cli *client.Client
)

func InitDockerClient() (err error) {
	Cli, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	return err
}

func CloseDockerClient() {
	Cli.Close()
}
