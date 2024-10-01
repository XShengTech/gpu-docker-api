package models

type ContainerRun struct {
	ImageName      string   `json:"imageName"`
	ReplicaSetName string   `json:"replicaSetName"`
	GpuCount       int      `json:"gpuCount,omitempty"`
	CpuCount       int      `json:"cpuCount,omitempty"`
	Memory         string   `json:"memory,omitempty"` // KB, MB, GB, TB
	Binds          []Bind   `json:"binds,omitempty"`
	Env            []string `json:"env,omitempty"`
	Cmd            []string `json:"cmd,omitempty"`
	ContainerPorts []string `json:"containerPorts,omitempty"`
}

type GpuPatch struct {
	GpuCount int `json:"gpuCount"`
}

type CpuPatch struct {
	CpuCount int `json:"cpuCount"`
}

type MemoryPatch struct {
	Memory string `json:"memory"` // KB, MB, GB, TB
}

type VolumePatch struct {
	OldBind *Bind `json:"oldBind"`
	NewBind *Bind `json:"newBind"`
}

type PatchRequest struct {
	GpuPatch    *GpuPatch    `json:"gpuPatch"`
	CpuPatch    *CpuPatch    `json:"cpuPatch"`
	MemoryPatch *MemoryPatch `json:"memoryPatch"`
	VolumePatch *VolumePatch `json:"volumePatch"`
}

type RollbackRequest struct {
	Version int64 `json:"version"`
}

type ContainerExecute struct {
	WorkDir string   `json:"workDir,omitempty"`
	Cmd     []string `json:"cmd,omitempty"`
}

type ContainerCommit struct {
	NewImageName string `json:"newImageName"`
}

type ContainerHistoryItem struct {
	Version    int64             `json:"version"`
	CreateTime string            `json:"createTime"`
	Status     EtcdContainerInfo `json:"status"`
}
