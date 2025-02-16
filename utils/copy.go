package utils

import (
	"context"
	"fmt"

	"github.com/commander-cli/cmd"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"

	"github.com/mayooot/gpu-docker-api/internal/docker"
)

var (
	cpRFPOption = "(cd %s; tar c .) | (cd %s; tar x)"
)

func CopyDir(src, dest string) error {
	command := fmt.Sprintf(cpRFPOption, src, dest)
	if err := cmd.NewCommand(command).Execute(); err != nil {
		return errors.Wrapf(err, "cmd.Execute failed, command %s, src:%s, dest: %s", command, src, dest)
	}
	return nil
}

// CopyOldMergedToNewContainerMerged is used to copy the merged layer from the old container
// to the new container during patch operations.
func CopyOldMergedToNewContainerMerged(oldContainer, newContainer string) error {
	oldMerged, err := GetContainerMergedLayer(oldContainer)
	if err != nil {
		return errors.WithMessage(err, "GetContainerMergedLayer failed")
	}
	newMerged, err := GetContainerMergedLayer(newContainer)
	if err != nil {
		return errors.WithMessage(err, "GetContainerMergedLayer failed")
	}

	if err = CopyDir(oldMerged, newMerged); err != nil {
		return errors.WithMessage(err, "copyDir failed")
	}

	return nil
}

func GetContainerMergedLayer(name string) (string, error) {
	resp, err := docker.Cli.ContainerInspect(context.TODO(), name)
	if err != nil || len(resp.GraphDriver.Data["UpperDir"]) == 0 {
		return "", errors.Wrapf(err, "docker.ContainerInspect failed, name: %s", name)
	}
	return resp.GraphDriver.Data["UpperDir"], nil
}

// CopyOldMountPointToContainerMountPoint is used to copy the volume data from the old container
// to the new container during patch operations.
func CopyOldMountPointToContainerMountPoint(oldVolume, newVolume string) error {
	if err := moveVolumeData(oldVolume, newVolume); err != nil {
		return errors.WithMessage(err, "moveData failed")
	}
	return nil
}

func GetVolumeMountPoint(name string) (string, error) {
	ctx := context.Background()
	resp, err := docker.Cli.VolumeInspect(ctx, name)
	if err != nil || len(resp.Mountpoint) == 0 {
		return "", errors.Wrapf(err, "docker.VolumeInspect failed, name: %s", name)
	}
	return resp.Mountpoint, nil
}

func moveVolumeData(src, dest string) error {
	var (
		networkingConfig network.NetworkingConfig
		platform         ocispec.Platform
	)

	hostConfig := container.HostConfig{
		Binds: []string{
			fmt.Sprintf("%s:/root/src", src),
			fmt.Sprintf("%s:/root/dest", dest),
		},
	}

	ctx := context.Background()

	resp, err := docker.Cli.ContainerCreate(ctx, &container.Config{
		Image: "ubuntu:22.04",
		Cmd:   []string{"tail", "-f", "/dev/null"},
	}, &hostConfig, &networkingConfig, &platform, "")
	if err != nil {
		return errors.Wrapf(err, "docker.ContainerCreate failed, src: %s, dest: %s", src, dest)
	}

	defer func() {
		docker.Cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
	}()

	if err = docker.Cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return errors.Wrapf(err, "docker.ContainerStart failed, container: %s", resp.ID)
	}

	execCreate, err := docker.Cli.ContainerExecCreate(ctx, resp.ID, types.ExecConfig{
		AttachStderr: true,
		AttachStdout: true,
		Detach:       true,
		DetachKeys:   "ctrl-p,q",
		WorkingDir:   "/root/",
		Cmd:          []string{"sh", "-c", "find /root/src/ -maxdepth 1 -type f | xargs mv --target-directory=/root/dest; mv /root/src/* /root/dest"},
	})
	if err != nil {
		return errors.Wrapf(err, "docker.ContainerExecCreate failed, container: %s", resp.ID)
	}

	err = docker.Cli.ContainerExecStart(ctx, execCreate.ID, types.ExecStartCheck{})
	if err != nil {
		return errors.Wrapf(err, "docker.ContainerExecAttach failed, container: %s", resp.ID)
	}

	return nil
}
