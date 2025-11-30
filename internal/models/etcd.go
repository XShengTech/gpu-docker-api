package models

import (
	"encoding/json"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type EtcdContainerInfo struct {
	Version          int64                     `json:"version"`
	CreateTime       string                    `json:"createTime"`
	Config           *container.Config         `json:"config"`
	HostConfig       *container.HostConfig     `json:"hostConfig"`
	NetworkingConfig *network.NetworkingConfig `json:"networkingConfig"`
	Platform         *ocispec.Platform         `json:"platform"`
	ContainerName    string                    `json:"containerName"`
}

func (i *EtcdContainerInfo) Serialize() *string {
	bytes, _ := json.Marshal(i)
	tmp := string(bytes)
	return &tmp
}

type EtcdVolumeInfo struct {
	Version    int64                       `json:"version"`
	CreateTime string                      `json:"createTime"`
	Opt        *client.VolumeCreateOptions `json:"opt"`
}

func (i *EtcdVolumeInfo) Serialize() *string {
	bytes, _ := json.Marshal(i)
	tmp := string(bytes)
	return &tmp
}
