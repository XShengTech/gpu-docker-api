package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"github.com/ngaut/log"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"

	"github.com/mayooot/gpu-docker-api/internal/docker"
	"github.com/mayooot/gpu-docker-api/internal/etcd"
	"github.com/mayooot/gpu-docker-api/internal/models"
	"github.com/mayooot/gpu-docker-api/internal/schedulers"
	vmap "github.com/mayooot/gpu-docker-api/internal/version"
	"github.com/mayooot/gpu-docker-api/internal/workQueue"
	"github.com/mayooot/gpu-docker-api/internal/xerrors"
	"github.com/mayooot/gpu-docker-api/utils"
)

const ballastStone = "var/backups/ballaststone"

var lxcfsBind = []string{
	"/var/lib/lxcfs/proc/cpuinfo:/proc/cpuinfo:rw",
	"/var/lib/lxcfs/proc/diskstats:/proc/diskstats:rw",
	"/var/lib/lxcfs/proc/meminfo:/proc/meminfo:rw",
	"/var/lib/lxcfs/proc/stat:/proc/stat:rw",
	"/var/lib/lxcfs/proc/swaps:/proc/swaps:rw",
	"/var/lib/lxcfs/proc/uptime:/proc/uptime:rw",
}

type ReplicaSetService struct{}

// RunGpuContainer just sets the parameters, the real run a container is in the `runContainer`
func (rs *ReplicaSetService) RunGpuContainer(spec *models.ContainerRun) (id, containerName string, err error) {
	var (
		config           container.Config
		hostConfig       container.HostConfig
		networkingConfig network.NetworkingConfig
		platform         ocispec.Platform
	)
	ctx := context.Background()

	if rs.existContainer(spec.ReplicaSetName) {
		return id, containerName, errors.Wrapf(xerrors.NewContainerExistedError(), "container %s", spec.ReplicaSetName)
	}

	config = container.Config{
		Image:     spec.ImageName,
		Cmd:       spec.Cmd,
		Env:       spec.Env,
		OpenStdin: true,
		Tty:       true,
	}

	// limit rootfs
	hostConfig.StorageOpt = map[string]string{
		"size": "30G",
	}
	shmSize, _ := utils.ToBytes("256GB")
	hostConfig.ShmSize = shmSize
	hostConfig.Runtime = rs.containerRuntime()

	// bind port
	if len(spec.ContainerPorts) > 0 {
		hostConfig.PortBindings = make(nat.PortMap, len(spec.ContainerPorts))
		config.ExposedPorts = make(nat.PortSet, len(spec.ContainerPorts))
		for _, port := range spec.ContainerPorts {
			config.ExposedPorts[nat.Port(port+"/tcp")] = struct{}{}
			hostConfig.PortBindings[nat.Port(port+"/tcp")] = nil
		}
	}

	// bind gpu resource
	var uuids []string
	if spec.GpuCount > 0 {
		uuids, err = schedulers.GpuScheduler.Apply(spec.GpuCount)
		if err != nil {
			return id, containerName, errors.Wrapf(err, "GpuScheduler.Apply failed, spec: %+v", spec)
		}
		hostConfig.Resources = rs.newContainerResource(uuids)
		log.Infof("services.RunGpuContainer, container: %s apply %d gpus, uuids: %+v", spec.ReplicaSetName+"-0", len(uuids), uuids)
	}

	// bind cpu resource
	if spec.CpuCount > 0 {
		cpusets, err := schedulers.CpuScheduler.Apply(spec.CpuCount)
		if err != nil {
			if spec.GpuCount > 0 {
				schedulers.GpuScheduler.Restore(uuids)
			}
			return id, containerName, errors.Wrapf(err, "CpuScheduler.Apply failed, spec: %+v", spec)
		}
		hostConfig.Resources.CpusetCpus = cpusets
	}

	// bind memory resource
	if spec.Memory != "" {
		memory, err := utils.ToBytes(spec.Memory)
		if err != nil {
			if spec.GpuCount > 0 {
				schedulers.GpuScheduler.Restore(uuids)
			}
			if spec.CpuCount > 0 {
				schedulers.CpuScheduler.Restore(strings.Split(hostConfig.Resources.CpusetCpus, ","))
			}
			return id, containerName, errors.Wrapf(err, "MemoryGetBytes failed, spec: %+v", spec)
		}
		hostConfig.Resources.Memory = memory
	}

	// bind volume
	hostConfig.Binds = make([]string, 0, len(spec.Binds)+len(lxcfsBind))
	for i := range spec.Binds {
		// Binds
		hostConfig.Binds = append(hostConfig.Binds, fmt.Sprintf("%s:%s", spec.Binds[i].Src, spec.Binds[i].Dest))
	}
	hostConfig.Binds = append(hostConfig.Binds, lxcfsBind...)

	// create and start
	id, containerName, kv, err := rs.runContainer(ctx, spec.ReplicaSetName, &models.EtcdContainerInfo{
		Config:           &config,
		HostConfig:       &hostConfig,
		NetworkingConfig: &networkingConfig,
		Platform:         &platform,
	}, false)
	if err != nil {
		if len(hostConfig.Resources.DeviceRequests) > 0 {
			schedulers.GpuScheduler.Restore(hostConfig.Resources.DeviceRequests[0].DeviceIDs)
		}
		schedulers.CpuScheduler.Restore(strings.Split(hostConfig.Resources.CpusetCpus, ","))
		return id, containerName, errors.Wrapf(err, "serivce.runContainer failed, spec: %+v", spec)
	}

	workQueue.Queue <- etcd.PutKeyValue{
		Resource: etcd.Containers,
		Key:      kv.Key,
		Value:    kv.Value,
	}
	return
}

func (rs *ReplicaSetService) DeleteContainer(name string) error {
	// get the latest version number
	version, ok := vmap.ContainerVersionMap.Get(name)
	if !ok {
		return errors.Errorf("container: %s version: %d not found in ContainerVersionMap", name, version)
	}

	ctrVersionName := fmt.Sprintf("%s-%d", name, version)

	running, err := rs.containerStatusRunning(ctrVersionName)
	if err != nil {
		return errors.WithMessage(err, "services.containerStatusRunning failed")
	}
	pause, err := rs.containerStatusPaused(ctrVersionName)
	if err != nil {
		return errors.WithMessage(err, "services.containerStatusPaused failed")
	}

	if running || pause {
		uuids, err := rs.containerDeviceRequestsDeviceIDs(ctrVersionName)
		if err != nil {
			return errors.WithMessage(err, "services.containerDeviceRequestsDeviceIDs failed")
		}
		schedulers.GpuScheduler.Restore(uuids)
		log.Infof("services.DeleteContainer, container: %s restore %d gpus, uuids: %+v",
			name, len(uuids), uuids)

		cpusets, err := rs.containerCpusetCpus(ctrVersionName)
		if err != nil {
			return errors.WithMessage(err, "services.containerCpusetCpus failed")
		}
		schedulers.CpuScheduler.Restore(cpusets)
		log.Infof("services.DeleteContainer, container: %s restore %d cpus, cpusets: %+v",
			name, len(cpusets), cpusets)

		ports, err := rs.containerPortBindings(ctrVersionName)
		if err != nil {
			return errors.WithMessage(err, "services.containerPortBindings failed")
		}
		schedulers.PortScheduler.Restore(ports)
		log.Infof("services.DeleteContainer, container: %s restore %d ports: %+v",
			name, len(ports), ports)
	}

	err = deleteMergeMap(name)
	if err != nil {
		return errors.WithMessage(err, "deleteMergeMap failed")
	}

	// delete the version number and asynchronously delete the container info in etcd
	vmap.ContainerVersionMap.Remove(strings.Split(name, "-")[0])
	workQueue.Queue <- etcd.DelKey{
		Resource: etcd.Containers,
		Key:      name,
	}

	err = docker.Cli.ContainerRemove(context.TODO(),
		fmt.Sprintf("%s-%d", name, version),
		container.RemoveOptions{Force: true})
	if err != nil {
		return errors.WithMessage(err, "docker.Cli.ContainerRemove failed")
	}

	log.Infof("services.DeleteContainer, container: %s delete successfully", fmt.Sprintf("%s-%d", name, version))
	log.Infof("services.DeleteContainer, container: %s will be del etcd info and version record", name)
	return nil
}

func (rs *ReplicaSetService) ExecuteContainer(name string, exec *models.ContainerExecute) (resp *string, err error) {
	// get the latest version number
	version, ok := vmap.ContainerVersionMap.Get(name)
	if !ok {
		return nil, errors.Errorf("container: %s version: %d not found in ContainerVersionMap", name, version)
	}

	workDir := "/"
	var cmd []string
	if len(exec.WorkDir) != 0 {
		workDir = exec.WorkDir
	}
	if len(exec.Cmd) != 0 {
		cmd = exec.Cmd
	}

	ctx := context.Background()
	execCreate, err := docker.Cli.ContainerExecCreate(ctx, fmt.Sprintf("%s-%d", name, version), container.ExecOptions{
		AttachStderr: true,
		AttachStdout: true,
		Detach:       true,
		DetachKeys:   "ctrl-p,q",
		WorkingDir:   workDir,
		Cmd:          cmd,
	})
	if err != nil {
		return resp, errors.Wrapf(err, "docker.ContainerExecCreate failed, name: %s, spec: %+v", name, exec)
	}

	hijackedResp, err := docker.Cli.ContainerExecAttach(ctx, execCreate.ID, container.ExecAttachOptions{})
	if err != nil {
		return resp, errors.Wrapf(err, "docker.ContainerExecAttach failed, name: %s, spec: %+v", name, exec)
	}
	defer hijackedResp.Close()

	var buf bytes.Buffer
	_, _ = stdcopy.StdCopy(&buf, &buf, hijackedResp.Reader)
	str := buf.String()
	resp = &str
	log.Infof("services.ExecuteContainer, container: %s execute successfully, exec: %+v", name, exec)
	return
}

func (rs *ReplicaSetService) PatchContainer(name string, spec *models.PatchRequest) (id, newContainerName string, err error) {
	// get the latest version number
	version, ok := vmap.ContainerVersionMap.Get(name)
	if !ok {
		return id, newContainerName, errors.Errorf("container: %s version: %d not found in ContainerVersionMap", name, version)
	}
	ctrVersionName := fmt.Sprintf("%s-%d", name, version)

	// get the container info
	ctx := context.Background()
	infoBytes, err := etcd.GetValue(etcd.Containers, name)
	if err != nil {
		return id, newContainerName, errors.Wrapf(err, "etcd.GetValue failed, key: %s", etcd.ResourcePrefix(etcd.Containers, name))
	}
	info := &models.EtcdContainerInfo{}
	if err = json.Unmarshal(infoBytes, &info); err != nil {
		return id, newContainerName, errors.WithMessage(err, "json.Unmarshal failed")
	}

	// update gpu info
	info, err = rs.patchGpu(ctrVersionName, spec.GpuPatch, info)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "patchGpu failed")
	}

	// update cpu info
	info, err = rs.patchCpu(ctrVersionName, spec.CpuPatch, info)
	if err != nil {
		if len(info.HostConfig.Resources.DeviceRequests) > 0 {
			schedulers.GpuScheduler.Restore(info.HostConfig.Resources.DeviceRequests[0].DeviceIDs)
		}
		return id, newContainerName, errors.WithMessage(err, "patchCpu failed")
	}

	// update memory info
	info, err = rs.patchMemory(ctrVersionName, spec.MemoryPatch, info)
	if err != nil {
		if len(info.HostConfig.Resources.DeviceRequests) > 0 {
			schedulers.GpuScheduler.Restore(info.HostConfig.Resources.DeviceRequests[0].DeviceIDs)
		}
		schedulers.CpuScheduler.Restore(strings.Split(info.HostConfig.Resources.CpusetCpus, ","))
		return id, newContainerName, errors.WithMessage(err, "patchMemory failed")
	}

	// update volume info
	info, err = rs.patchVolume(spec.VolumePatch, info)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "patchVolume failed")
	}

	// create a new container to replace the old one
	id, newContainerName, kv, err := rs.runContainer(ctx, name, info, true)
	if err != nil {
		if len(info.HostConfig.Resources.DeviceRequests) > 0 {
			schedulers.GpuScheduler.Restore(info.HostConfig.Resources.DeviceRequests[0].DeviceIDs)
		}
		schedulers.CpuScheduler.Restore(strings.Split(info.HostConfig.Resources.CpusetCpus, ","))
		return id, newContainerName, errors.WithMessage(err, "runContainer failed")
	}

	err = rs.containerRemoveBallastStone(ctrVersionName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "removeContainerBallastStone failed")
	}

	// copy the old container's merged files to the new container
	err = utils.CopyOldMergedToNewContainerMerged(ctrVersionName, newContainerName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "utils.CopyOldMergedToNewContainerMerged failed")
	}

	err = rs.startContainer(ctx, id, newContainerName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "startContainer failed")
	}

	// delete the old container
	// no gpu resources are returned because they are already returned when the gpu is lowered
	// or when upgrading the gpu, the original gpu will be used.
	err = setToMergeMap(ctrVersionName, version)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "setToMergeMap failed")
	}
	err = rs.DeleteContainerForUpdate(ctrVersionName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "DeleteContainerForUpdate failed")
	}

	workQueue.Queue <- etcd.PutKeyValue{
		Resource: etcd.Containers,
		Key:      kv.Key,
		Value:    kv.Value,
	}

	log.Infof("services.PatchContainer, container: %s patch configuration successfully", name)
	return
}

func (rs *ReplicaSetService) RollbackContainer(name string, spec *models.RollbackRequest) (string, error) {
	// check that the version to be rolled back is the same as the current version
	version, ok := vmap.ContainerVersionMap.Get(name)
	if !ok {
		return "", errors.Errorf("container: %s version: %d not found in ContainerVersionMap", name, version)
	}
	if spec.Version == version {
		return "", xerrors.NewNoRollbackRequiredError()
	}

	// get revision info form etcd
	value, err := etcd.GetRevision(etcd.Containers, name, spec.Version)
	if err != nil {
		return "", errors.WithMessage(err, "etcd.GetRevisionRange failed")
	}
	info := &models.EtcdContainerInfo{}
	if err = json.Unmarshal(value, &info); err != nil {
		return "", errors.WithMessage(err, "json.Unmarshal failed")
	}

	// compare gpu info
	ctrVersionName := fmt.Sprintf("%s-%d", name, version)
	gpucount := 0
	if len(info.HostConfig.Resources.DeviceRequests) > 0 {
		gpucount = len(info.HostConfig.Resources.DeviceRequests[0].DeviceIDs)
	}
	info, err = rs.patchGpu(ctrVersionName, &models.GpuPatch{
		GpuCount: gpucount,
	}, info)
	if err != nil {
		return "", errors.WithMessage(err, "patchGpu failed")
	}

	// compare cpu info
	info, err = rs.patchCpu(ctrVersionName, &models.CpuPatch{
		CpuCount: len(strings.Split(info.HostConfig.Resources.CpusetCpus, ",")),
	}, info)
	if err != nil {
		return "", errors.WithMessage(err, "patchCpu failed")
	}

	// compare memory info
	info, err = rs.patchMemory(ctrVersionName, &models.MemoryPatch{
		Memory: fmt.Sprintf("%dGB", info.HostConfig.Resources.Memory/1024/1024),
	}, info)
	if err != nil {
		return "", errors.WithMessage(err, "patchMemory failed")
	}

	// create a new container to replace the old one
	_, newContainerName, kv, err := rs.runContainer(context.TODO(), name, info, false)
	if err != nil {
		return "", errors.WithMessage(err, "runContainer failed")
	}

	// copy the old container's merged files to the new container
	err = utils.CopyOldMergedToNewContainerMerged(ctrVersionName, newContainerName)
	if err != nil {
		return "", errors.WithMessage(err, "utils.CopyOldMergedToNewContainerMerged failed")
	}

	// delete the old container
	// no gpu resources are returned because they are already returned when the gpu is lowered
	// or when upgrading the gpu, the original gpu will be used.
	err = setToMergeMap(ctrVersionName, version)
	if err != nil {
		return "", errors.WithMessage(err, "setToMergeMap failed")
	}
	err = rs.DeleteContainerForUpdate(ctrVersionName)
	if err != nil {
		return "", errors.WithMessage(err, "DeleteContainerForUpdate failed")
	}

	workQueue.Queue <- etcd.PutKeyValue{
		Resource: etcd.Containers,
		Key:      kv.Key,
		Value:    kv.Value,
	}

	log.Infof("services.RollbackContainer, container: %s patch configuration successfully", ctrVersionName)
	return newContainerName, nil
}

func (rs *ReplicaSetService) patchGpu(name string, spec *models.GpuPatch, info *models.EtcdContainerInfo) (*models.EtcdContainerInfo, error) {
	running, err := rs.containerStatusRunning(name)
	if err != nil {
		return info, errors.WithMessage(err, "services.containerStatusRunning failed")
	}
	pause, err := rs.containerStatusPaused(name)
	if err != nil {
		return info, errors.WithMessage(err, "services.containerStatusPaused failed")
	}

	uuids, err := rs.containerDeviceRequestsDeviceIDs(name)
	if err != nil {
		return info, errors.WithMessage(err, "services.containerDeviceRequestsDeviceIDs failed")
	}

	if spec != nil {
		if len(uuids) == spec.GpuCount && (running || pause) {
			return info, nil
		}
	}

	if spec == nil {
		spec = &models.GpuPatch{
			GpuCount: len(uuids),
		}
	}

	if running || pause {
		schedulers.GpuScheduler.Restore(uuids)
		log.Infof("services.PatchContainerGpuInfo, container: %s restore %d gpus, uuids: %+v",
			name, len(uuids), uuids)
	}
	if spec.GpuCount == 0 {
		info.HostConfig.Resources = container.Resources{
			Memory: info.HostConfig.Memory,
		}
	} else {
		uuids, err = schedulers.GpuScheduler.Apply(spec.GpuCount)
		if err != nil {
			return info, errors.WithMessage(err, "GpuScheduler.Apply failed")
		}
		log.Infof("services.PatchContainerGpuInfo, container: %s apply %d gpus, uuids: %+v", name, spec.GpuCount, uuids)
		cr := rs.newContainerResource(uuids)
		info.HostConfig.Resources.DeviceRequests = cr.DeviceRequests
	}

	return info, nil
}

func (rs *ReplicaSetService) patchCpu(name string, spec *models.CpuPatch, info *models.EtcdContainerInfo) (*models.EtcdContainerInfo, error) {
	running, err := rs.containerStatusRunning(name)
	if err != nil {
		return info, errors.WithMessage(err, "services.containerStatusRunning failed")
	}
	pause, err := rs.containerStatusPaused(name)
	if err != nil {
		return info, errors.WithMessage(err, "services.containerStatusPaused failed")
	}

	cpuset, err := rs.containerCpusetCpus(name)
	if err != nil {
		return info, errors.WithMessage(err, "services.containerDeviceRequestsDeviceIDs failed")
	}

	if spec != nil {
		if len(cpuset) == spec.CpuCount && (running || pause) {
			return info, nil
		}
	}

	if spec == nil {
		spec = &models.CpuPatch{
			CpuCount: len(cpuset),
		}
	}

	if running || pause {
		schedulers.CpuScheduler.Restore(cpuset)
		log.Infof("services.PatchContainerCpuInfo, container: %s restore %d cpus, cpusets: %+v",
			name, len(cpuset), cpuset)
	}
	cpusets, err := schedulers.CpuScheduler.Apply(spec.CpuCount)
	if err != nil {
		return info, errors.WithMessage(err, "CpuScheduler.Apply failed")
	}
	log.Infof("services.PatchContainerCpuInfo, container: %s apply %d cpus, cpusets: %+v", name, spec.CpuCount, cpusets)
	info.HostConfig.Resources.CpusetCpus = strings.TrimLeft(strings.Trim(cpusets, ","), ",")
	log.Infof("services.PatchContainerCpuInfo, container: %s upgrad %d cpu configuration, now use %d cpus, cpusets: %+v",
		name, len(cpuset), len(strings.Split(info.HostConfig.Resources.CpusetCpus, ",")), info.HostConfig.Resources.CpusetCpus)

	return info, nil
}

func (rs *ReplicaSetService) patchMemory(name string, spec *models.MemoryPatch, info *models.EtcdContainerInfo) (*models.EtcdContainerInfo, error) {
	if spec == nil {
		return info, nil
	}
	memory, err := rs.containerMemory(name)
	if err != nil {
		return info, errors.WithMessage(err, "services.containerMemory failed")
	}

	applymemory, err := utils.ToBytes(spec.Memory)
	if err != nil {
		return info, errors.WithMessage(err, "models.MemoryGetBytes failed")
	}

	if memory == applymemory {
		return info, nil
	}

	info.HostConfig.Resources.Memory = applymemory

	return info, nil
}

func (rs *ReplicaSetService) patchVolume(spec *models.VolumePatch, info *models.EtcdContainerInfo) (*models.EtcdContainerInfo, error) {
	if spec == nil {
		return info, nil
	}

	if spec.OldBind.Format() == spec.NewBind.Format() {
		return info, nil
	}

	for i := range info.HostConfig.Binds {
		if info.HostConfig.Binds[i] == spec.OldBind.Format() {
			info.HostConfig.Binds[i] = spec.NewBind.Format()
			break
		}
	}
	return info, nil
}

func (rs *ReplicaSetService) StopContainer(name string, restoreGpu, restoreCpu, restorePort, isLatest bool) error {
	var err error

	if isLatest {
		// get the latest version number
		version, ok := vmap.ContainerVersionMap.Get(name)
		if !ok {
			return errors.Errorf("container: %s version: %d not found in ContainerVersionMap", name, version)
		}
		name = fmt.Sprintf("%s-%d", name, version)
	}

	// whether to restore gpu resources
	var uuids []string
	if restoreGpu {
		uuids, err = rs.containerDeviceRequestsDeviceIDs(name)
		if err != nil {
			return errors.WithMessage(err, "services.containerDeviceRequestsDeviceIDs failed")
		}
		schedulers.GpuScheduler.Restore(uuids)
		log.Infof("services.StopContainer, container: %s restore %d gpus, uuids: %+v",
			name, len(uuids), uuids)
	}

	// whether to restore cpu resources
	var cpusets []string
	if restoreCpu {
		cpusets, err = rs.containerCpusetCpus(name)
		if err != nil {
			return errors.WithMessage(err, "services.containerCpusetCpus failed")
		}
		schedulers.CpuScheduler.Restore(cpusets)
		log.Infof("services.StopContainer, container: %s restore %d cpus, cpusets: %+v",
			name, len(cpusets), cpusets)
	}

	// whether to restore port resources
	if restorePort {
		ports, err := rs.containerPortBindings(name)
		if err != nil {
			return errors.WithMessage(err, "services.containerPortBindings failed")
		}
		schedulers.PortScheduler.Restore(ports)
		log.Infof("services.StopContainer, container: %s restore %d ports: %+v",
			name, len(ports), ports)
	}

	// stop container
	ctx := context.Background()
	if err := docker.Cli.ContainerStop(ctx, name, container.StopOptions{}); err != nil {
		schedulers.GpuScheduler.Restore(uuids)
		schedulers.CpuScheduler.Restore(cpusets)
		return errors.WithMessage(err, "docker.ContainerStop failed")
	}

	log.Infof("services.StopContainer, container: %s stop successfully", name)
	return nil
}

func (rs *ReplicaSetService) PauseContainer(name string) error {
	var err error

	version, ok := vmap.ContainerVersionMap.Get(name)
	if !ok {
		return errors.Errorf("container: %s version: %d not found in ContainerVersionMap", name, version)
	}
	name = fmt.Sprintf("%s-%d", name, version)

	ctx := context.Background()
	if err = docker.Cli.ContainerPause(ctx, name); err != nil {
		log.Errorf("services.PauseContainer, container: %s pause failed, err: %v", name, err)
		return errors.WithMessage(err, "docker.ContainerPause failed")
	}

	log.Infof("services.PauseContainer, container: %s pause successfully", name)
	return nil
}

func (rs *ReplicaSetService) DeleteContainerForUpdate(name string) error {
	// restore port resources
	ports, err := rs.containerPortBindings(name)
	if err != nil {
		return errors.WithMessage(err, "services.containerPortBindings failed")
	}
	schedulers.PortScheduler.Restore(ports)
	log.Infof("services.DeleteContainerForUpdate, container: %s restore %d ports: %+v",
		name, len(ports), ports)

	// delete container
	err = docker.Cli.ContainerRemove(context.TODO(),
		name,
		container.RemoveOptions{Force: true})
	if err != nil {
		return errors.WithMessage(err, "docker.ContainerRemove failed")
	}

	return nil
}

func setToMergeMap(name string, version int64) error {
	var err error
	defer func() {
		if err != nil {
			vmap.ContainerMergeMap.Remove(version)
		}
	}()

	// mergedDir, err := utils.GetContainerMergedLayer(name)
	// if err != nil {
	// 	return errors.WithMessagef(err, "utils.GetContainerMergedLayer failed, container: %s", name)
	// }
	layer := "merges"
	dir, _ := os.Getwd()
	path := filepath.Join(dir, layer, strings.Split(name, "-")[0], name)
	_ = os.MkdirAll(path, 0755)

	// err = utils.CopyDir(mergedDir, path)
	// if err != nil {
	// 	return errors.WithMessagef(err, "utils.CopyDir failed, container: %s", name)
	// }
	vmap.ContainerMergeMap.Set(version, path)
	return nil
}

func deleteMergeMap(name string) error {
	layer := "merges"
	dir, _ := os.Getwd()
	path := filepath.Join(dir, layer, strings.Split(name, "-")[0])
	err := os.RemoveAll(path)
	if err != nil {
		return errors.WithMessagef(err, "remove container merge layer failed, path: %s", path)
	}
	return nil
}

func (rs *ReplicaSetService) StartupContainer(name string) error {
	// get the latest version number
	version, ok := vmap.ContainerVersionMap.Get(name)
	if !ok {
		return errors.Errorf("container: %s version: %d not found in ContainerVersionMap", name, version)
	}

	err := docker.Cli.ContainerRestart(context.TODO(),
		fmt.Sprintf("%s-%d", name, version),
		container.StopOptions{})
	if err != nil {
		return errors.WithMessagef(err, "docker.ContainerRestart failed, name: %s", name)
	}

	return nil
}

// RestartContainer will reapply gpu and port,
// but the logic for applying port is in the runContainer function
func (rs *ReplicaSetService) RestartContainer(name string) (id, newContainerName string, err error) {
	// get the latest version number
	version, ok := vmap.ContainerVersionMap.Get(name)
	if !ok {
		return id, newContainerName, errors.Errorf("container: %s version: %d not found in ContainerVersionMap", name, version)
	}
	ctrVersionName := fmt.Sprintf("%s-%d", name, version)

	running, err := rs.containerStatusRunning(ctrVersionName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "services.containerStatusRunning failed")
	}
	pause, err := rs.containerStatusPaused(ctrVersionName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "services.containerStatusPaused failed")
	}

	// get info about used gpus
	ctx := context.Background()
	uuids, err := rs.containerDeviceRequestsDeviceIDs(ctrVersionName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "services.containerDeviceRequestsDeviceIDs failed")
	}

	// get info about used cpus
	cpus, err := rs.containerCpusetCpus(ctrVersionName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "services.containerCpusetCpus failed")
	}

	// get memory info
	memory, err := rs.containerMemory(ctrVersionName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "services.containerMemory failed")
	}

	// get creation info from etcd
	infoBytes, err := etcd.GetValue(etcd.Containers, name)
	if err != nil {
		return id, newContainerName, errors.Wrapf(err, "etcd.GetValue failed, key: %s", etcd.ResourcePrefix(etcd.Containers, name))
	}
	info := &models.EtcdContainerInfo{}
	if err = json.Unmarshal(infoBytes, &info); err != nil {
		return id, newContainerName, errors.WithMessage(err, "json.Unmarshal failed")
	}

	// check whether the container is using gpu
	if len(uuids) != 0 {
		if running || pause {
			schedulers.GpuScheduler.Restore(uuids)
		}
		// apply for gpu
		availableGpus, err := schedulers.GpuScheduler.Apply(len(uuids))
		if err != nil {
			return id, newContainerName, errors.WithMessage(err, "GpuScheduler.Apply failed")
		}
		log.Infof("services.RestartContainer, container: %s apply %d gpus, uuids: %+v", ctrVersionName, len(availableGpus), availableGpus)
		info.HostConfig.Resources = rs.newContainerResource(availableGpus)
	}

	// check whether the container is using cpu
	if len(cpus) != 0 {
		if running || pause {
			schedulers.CpuScheduler.Restore(cpus)
		}
		// apply for cpu
		availableCpus, err := schedulers.CpuScheduler.Apply(len(cpus))
		if err != nil {
			return id, newContainerName, errors.WithMessage(err, "CpuScheduler.Apply failed")
		}
		log.Infof("services.RestartContainer, container: %s apply %d cpus, cpusets: %+v", ctrVersionName, len(strings.Split(availableCpus, ",")), availableCpus)
		info.HostConfig.Resources.CpusetCpus = availableCpus
	}

	// check whether the container is using memory
	if memory != 0 {
		info.HostConfig.Resources.Memory = memory
	}

	//  create a container to replace the old one
	id, newContainerName, kv, err := rs.runContainer(ctx, name, info, true)
	if err != nil {
		if len(info.HostConfig.Resources.DeviceRequests) > 0 {
			schedulers.GpuScheduler.Restore(info.HostConfig.Resources.DeviceRequests[0].DeviceIDs)
		}
		schedulers.CpuScheduler.Restore(strings.Split(info.HostConfig.Resources.CpusetCpus, ","))
		return id, newContainerName, errors.WithMessage(err, "services.runContainer failed")
	}

	err = rs.containerRemoveBallastStone(ctrVersionName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "removeContainerBallastStone failed")
	}

	// copy the old container's merged files to the new container
	err = utils.CopyOldMergedToNewContainerMerged(ctrVersionName, newContainerName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "utils.CopyOldMergedToNewContainerMerged failed")
	}

	// start the new container
	err = rs.startContainer(ctx, id, newContainerName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "startContainer failed")
	}

	// delete the old container
	// no gpu resources are returned because they are already returned when the gpu is lowered
	// or when upgrading the gpu, the original gpu will be used.
	err = setToMergeMap(ctrVersionName, version)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "setToMergeMap failed")
	}
	err = rs.DeleteContainerForUpdate(ctrVersionName)
	if err != nil {
		return id, newContainerName, errors.WithMessage(err, "DeleteContainerForUpdate failed")
	}

	workQueue.Queue <- etcd.PutKeyValue{
		Resource: etcd.Containers,
		Key:      kv.Key,
		Value:    kv.Value,
	}

	log.Infof("services.RestartContainer, container restart successfully, "+
		"old container name: %s, new container name: %s, ",
		ctrVersionName, newContainerName)
	return
}

func (rs *ReplicaSetService) CommitContainer(name string, spec models.ContainerCommit) (imageName string, err error) {
	// get the latest version number
	version, ok := vmap.ContainerVersionMap.Get(name)
	if !ok {
		return imageName, errors.Errorf("container: %s version: %d not found in ContainerVersionMap", name, version)
	}

	// commit image
	ctx := context.Background()
	resp, err := docker.Cli.ContainerCommit(ctx, fmt.Sprintf("%s-%d", name, version), container.CommitOptions{
		Comment: fmt.Sprintf("container name %s, commit time: %s", fmt.Sprintf("%s-%d", name, version), time.Now().Format("2006-01-02 15:04:05")),
	})
	if err != nil {
		return imageName, errors.WithMessage(err, "docker.ContainerRestart failed")
	}

	// tag
	if len(spec.NewImageName) != 0 {
		imageName = spec.NewImageName
	}
	if err = docker.Cli.ImageTag(ctx, resp.ID, imageName); err != nil {
		return imageName, errors.WithMessage(err, "docker.ImageTag failed")
	}
	log.Infof("services.CommitContainer, container: %s commit successfully", fmt.Sprintf("%s-%d", name, version))
	return imageName, err
}

func (rs *ReplicaSetService) GetContainerInfo(name string) (info models.EtcdContainerInfo, err error) {
	infoBytes, err := etcd.GetValue(etcd.Containers, name)
	if err != nil {
		return info, errors.Wrapf(err, "etcd.GetValue failed, key: %s", etcd.ResourcePrefix(etcd.Containers, name))
	}

	if err = json.Unmarshal(infoBytes, &info); err != nil {
		return info, errors.WithMessage(err, "json.Unmarshal failed")
	}
	return
}

func (rs *ReplicaSetService) GetContainerHistory(name string) ([]*models.ContainerHistoryItem, error) {
	replicaSet, err := etcd.GetRevisionRange(etcd.Containers, name)
	if err != nil {
		return nil, errors.Wrapf(err, "etcd.GetRevisionRange failed, key: %s",
			etcd.ResourcePrefix(etcd.Containers, name))
	}

	resp := make([]*models.ContainerHistoryItem, 0, len(replicaSet))
	for _, combine := range replicaSet {
		var info models.EtcdContainerInfo
		err := json.Unmarshal(combine.Value, &info)
		if err != nil {
			return nil, errors.Wrapf(err, "json.Unmarshal failed, value: %s", combine.Value)
		}
		resp = append(resp, &models.ContainerHistoryItem{
			Version:    combine.Version,
			CreateTime: info.CreateTime,
			Status:     info,
		})
	}
	return resp, nil
}

func (rs *ReplicaSetService) startContainer(ctx context.Context, respId, ctrVersionName string) error {
	if err := docker.Cli.ContainerStart(ctx, respId, container.StartOptions{}); err != nil {
		_ = docker.Cli.ContainerRemove(ctx, respId, container.RemoveOptions{Force: true})
		return errors.Wrapf(err, "docker.ContainerStart failed, id: %s, name: %s", respId, ctrVersionName)
	}

	go func(name string) {
		time.Sleep(5 * time.Second)
		err := rs.containerCreateBallastStone(name)
		if err != nil {
			log.Errorf("services.containerCreateBallastStone failed, name: %s, err: %v", name, err)
		}
	}(ctrVersionName)

	return nil
}

// Check whether the container exists
func (rs *ReplicaSetService) existContainer(name string) bool {
	ctx := context.Background()
	list, err := docker.Cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.KeyValuePair{Key: "name", Value: fmt.Sprintf("^%s-", name)}),
	})
	if err != nil || len(list) == 0 {
		return false
	}

	return len(list) > 0
}

func (rs *ReplicaSetService) containerStatusRunning(name string) (bool, error) {
	ctx := context.Background()
	resp, err := docker.Cli.ContainerInspect(ctx, name)
	if err != nil {
		return false, errors.Wrapf(err, "docker.ContainerInspect failed, name: %s", name)
	}
	return resp.State.Running, nil
}

func (rs *ReplicaSetService) containerStatusPaused(name string) (bool, error) {
	ctx := context.Background()
	resp, err := docker.Cli.ContainerInspect(ctx, name)
	if err != nil {
		return false, errors.Wrapf(err, "docker.ContainerInspect failed, name: %s", name)
	}
	return resp.State.Paused, nil
}

func (rs *ReplicaSetService) containerPortBindings(name string) ([]string, error) {
	ctx := context.Background()
	resp, err := docker.Cli.ContainerInspect(ctx, name)
	if err != nil {
		return nil, errors.Wrapf(err, "docker.ContainerInspect failed, name: %s", name)
	}
	if resp.HostConfig.PortBindings == nil {
		return []string{}, nil
	}
	var ports []string
	for _, v := range resp.HostConfig.PortBindings {
		ports = append(ports, v[0].HostPort)
	}
	return ports, nil
}

func (rs *ReplicaSetService) containerCpusetCpus(name string) ([]string, error) {
	ctx := context.Background()
	resp, err := docker.Cli.ContainerInspect(ctx, name)
	if err != nil {
		return []string{}, errors.Wrapf(err, "docker.ContainerInspect failed, name: %s", name)
	}
	return strings.Split(resp.HostConfig.Resources.CpusetCpus, ","), nil
}

func (rs *ReplicaSetService) containerMemory(name string) (int64, error) {
	ctx := context.Background()
	resp, err := docker.Cli.ContainerInspect(ctx, name)
	if err != nil {
		return 0, errors.Wrapf(err, "docker.ContainerInspect failed, name: %s", name)
	}
	return resp.HostConfig.Resources.Memory, nil
}

func (rs *ReplicaSetService) containerCreateBallastStone(name string) error {
	containerName := strings.Split(name, "-")[0]

	cmds := models.ContainerExecute{
		Cmd: []string{
			"dd",
			"if=/dev/zero",
			"of=/" + ballastStone,
			"bs=1M",
			"count=5", // 5MB
		},
	}

	_, err := rs.ExecuteContainer(containerName, &cmds)
	if err != nil {
		return errors.WithMessagef(err, "services.ExecuteContainer failed, container: %s", containerName)
	}

	return nil
}

func (rs *ReplicaSetService) containerRemoveBallastStone(name string) error {
	mergedLayer, err := utils.GetContainerMergedLayer(name)
	if err != nil {
		return errors.WithMessagef(err, "utils.GetContainerMergedLayer failed, container: %s", name)
	}

	err = os.Remove(mergedLayer + "/" + ballastStone)
	if err != nil {
		if !os.IsNotExist(err) {
			return errors.WithMessagef(err, "remove container ballast stone failed, path: %s", mergedLayer+"/"+ballastStone)
		}
	}
	return nil
}
