//go:build !mock

package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/ngaut/log"
	"github.com/pkg/errors"

	"github.com/mayooot/gpu-docker-api/internal/docker"
	"github.com/mayooot/gpu-docker-api/internal/etcd"
	"github.com/mayooot/gpu-docker-api/internal/models"
	"github.com/mayooot/gpu-docker-api/internal/schedulers"
	vmap "github.com/mayooot/gpu-docker-api/internal/version"
)

// It will only be executed based on the `docker.client.ContainerCreate`
func (rs *ReplicaSetService) runContainer(ctx context.Context, name string, info *models.EtcdContainerInfo, onlyCreate bool) (string, string, etcd.PutKeyValue, error) {
	// set the version number
	version, _ := vmap.ContainerVersionMap.Get(name)
	version = version + 1
	vmap.ContainerVersionMap.Set(name, version)

	// add the version number to the env
	isExist := false
	for i := range info.Config.Env {
		if strings.HasPrefix(info.Config.Env[i], "CONTAINER_VERSION=") {
			isExist = true
			info.Config.Env[i] = fmt.Sprintf("CONTAINER_VERSION=%d", version)
			break
		}
	}
	if !isExist {
		info.Config.Env = append(info.Config.Env, fmt.Sprintf("CONTAINER_VERSION=%d", version))
	}

	var err error
	defer func() {
		// if run container failed, clear the version number
		if err != nil {
			if version == 1 {
				vmap.ContainerVersionMap.Remove(name)
			} else {
				vmap.ContainerVersionMap.Set(name, version-1)
			}
		}
	}()

	// apply for some host port
	if info.HostConfig.PortBindings != nil && len(info.HostConfig.PortBindings) > 0 {
		availableOSPorts, err := schedulers.PortScheduler.Apply(len(info.HostConfig.PortBindings))
		if err != nil {
			return "", "", etcd.PutKeyValue{}, errors.Wrapf(err, "Portscheduler.Apply failed, info: %+v", info)
		}
		var index int
		for k := range info.HostConfig.PortBindings {
			info.HostConfig.PortBindings[k] = []nat.PortBinding{{
				HostPort: availableOSPorts[index],
			}}
			index++
		}
	}

	// generate container name with version and save creation time
	ctrVersionName := fmt.Sprintf("%s-%d", name, version)
	info.CreateTime = time.Now().Format("2006-01-02 15:04:05")

	// create container
	resp, err := docker.Cli.ContainerCreate(ctx, info.Config, info.HostConfig, info.NetworkingConfig, info.Platform, ctrVersionName)
	if err != nil {
		return "", "", etcd.PutKeyValue{}, errors.Wrapf(err, "docker.ContainerCreate failed, name: %s", ctrVersionName)
	}

	if !onlyCreate {
		// start container
		if err = rs.startContainer(ctx, resp.ID, ctrVersionName); err != nil {
			return "", "", etcd.PutKeyValue{}, errors.Wrapf(err, "docker.ContainerStart failed, id: %s, name: %s", resp.ID, ctrVersionName)
		}
	}

	// creation info is added to etcd asynchronously
	val := &models.EtcdContainerInfo{
		Config:           info.Config,
		HostConfig:       info.HostConfig,
		NetworkingConfig: info.NetworkingConfig,
		Platform:         info.Platform,
		ContainerName:    ctrVersionName,
		Version:          version,
		CreateTime:       info.CreateTime,
	}

	log.Infof("services.runContainer, container: %s run successfully", ctrVersionName)
	return resp.ID,
		ctrVersionName,
		etcd.PutKeyValue{
			Resource: etcd.Containers,
			Key:      name,
			Value:    val.Serialize(),
		},
		nil
}

func (rs *ReplicaSetService) containerDeviceRequestsDeviceIDs(name string) ([]string, error) {
	ctx := context.Background()
	resp, err := docker.Cli.ContainerInspect(ctx, name)
	if err != nil {
		return nil, errors.Wrapf(err, "docker.ContainerInspect failed, name: %s", name)
	}
	if resp.HostConfig.DeviceRequests == nil {
		return []string{}, nil
	}
	return resp.HostConfig.DeviceRequests[0].DeviceIDs, nil
}

func (rs *ReplicaSetService) newContainerResource(uuids []string) container.Resources {
	return container.Resources{
		DeviceRequests: []container.DeviceRequest{{
			Driver:       "cdi",
			DeviceIDs:    uuids,
			Capabilities: nil,
			Options:      nil,
		}}}
}

func (rs *ReplicaSetService) containerRuntime() string {
	return "nvidia"
}
