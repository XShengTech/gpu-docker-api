package schedulers

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/commander-cli/cmd"
	"github.com/mayooot/gpu-docker-api/internal/etcd"
	"github.com/mayooot/gpu-docker-api/internal/workQueue"
	"github.com/mayooot/gpu-docker-api/internal/xerrors"
	"github.com/pkg/errors"
)

const (
	allCpuProcessorsCommand = "cat /proc/cpuinfo | grep 'processor' | wc -l"

	cpuStatusMapKey = "cpuStatusMapKey"
)

var CpuScheduler *cpuScheduler

type cpuScheduler struct {
	sync.RWMutex

	AvailableCpuNums int             `json:"availableCpuNums"`
	CpuStatusMap     map[string]byte `json:"cpuStatusMap"`
}

func InitCpuScheduler() error {
	var err error
	CpuScheduler, err = initCpuFormEtcd()
	if err != nil {
		return errors.Wrap(err, "initFormEtcd failed")
	}

	if CpuScheduler.AvailableCpuNums == 0 || len(CpuScheduler.CpuStatusMap) == 0 {
		// if it has not been initialized
		cpus, err := getAllCpuProcessors()
		if err != nil {
			return errors.Wrap(err, "getAllCpuProcessors failed")
		}

		CpuScheduler.AvailableCpuNums = len(cpus)
		for i := 0; i < len(cpus); i++ {
			CpuScheduler.CpuStatusMap[cpus[i]] = 0
		}
	}
	return nil
}

func CloseCpuScheduler() error {
	return etcd.Put(etcd.Cpus, cpuStatusMapKey, CpuScheduler.serialize())
}

func initCpuFormEtcd() (c *cpuScheduler, err error) {
	bytes, err := etcd.GetValue(etcd.Cpus, cpuStatusMapKey)
	if err != nil {
		if xerrors.IsNotExistInEtcdError(err) {
			err = nil
		} else {
			return c, err
		}
	}

	c = &cpuScheduler{
		CpuStatusMap: make(map[string]byte),
	}
	if len(bytes) != 0 {
		err = json.Unmarshal(bytes, &c)
	}
	return c, err
}

func (cs *cpuScheduler) Apply(num int) (string, error) {
	if num <= 0 || num > cs.AvailableCpuNums {
		return "", errors.New("num must be greater than 0 and less than " + strconv.Itoa(cs.AvailableCpuNums))
	}

	cs.Lock()
	defer cs.Unlock()

	keys := make([]int, 0, len(cs.CpuStatusMap))
	for k := range cs.CpuStatusMap {
		ki, _ := strconv.Atoi(k)
		keys = append(keys, ki)
	}

	sort.Ints(keys)

	var applyCpus []string
	for k := range keys {
		ks := strconv.Itoa(k)
		v := cs.CpuStatusMap[ks]
		if v == 0 {
			cs.CpuStatusMap[ks] = 1
			applyCpus = append(applyCpus, ks)
			if len(applyCpus) == num {
				break
			}
		}
	}

	if len(applyCpus) < num {
		cs.restore(applyCpus)
		return "", xerrors.NewCpuNotEnoughError()
	}

	cpuSet := strings.Trim(strings.Join(applyCpus, ","), ",")

	go cs.putToEtcd()

	return cpuSet, nil
}

func (cs *cpuScheduler) Restore(cpuSet []string) error {

	cs.Lock()
	defer cs.Unlock()

	err := cs.restore(cpuSet)
	if err != nil {
		return errors.Wrap(err, "restore failed")
	}
	go cs.putToEtcd()

	return nil
}

func (cs *cpuScheduler) restore(cpuSet []string) error {
	for _, cpu := range cpuSet {
		cs.CpuStatusMap[cpu] = 0
	}

	return nil
}

func (cs *cpuScheduler) serialize() *string {
	cs.RLock()
	defer cs.RUnlock()

	bytes, _ := json.Marshal(cs)
	tmp := string(bytes)
	return &tmp
}

func (cs *cpuScheduler) GetCpuStatus() map[string]byte {
	cs.RLock()
	defer cs.RUnlock()

	copyMap := make(map[string]byte, len(cs.CpuStatusMap))
	for k, v := range cs.CpuStatusMap {
		copyMap[k] = v
	}

	return copyMap
}

func (cs *cpuScheduler) putToEtcd() {
	workQueue.Queue <- etcd.PutKeyValue{
		Resource: etcd.Cpus,
		Key:      cpuStatusMapKey,
		Value:    CpuScheduler.serialize(),
	}
}

func getAllCpuProcessors() ([]string, error) {
	c := cmd.NewCommand(allCpuProcessorsCommand)
	err := c.Execute()
	if err != nil {
		return nil, errors.Wrap(err, "cmd.Execute failed")
	}

	var cpuList []string
	cpuNum, err := strconv.Atoi(strings.Trim(c.Stdout(), "\n"))
	if err != nil {
		return nil, errors.Wrap(err, "strconv.Atoi failed")
	}
	for i := 0; i < cpuNum; i++ {
		cpuList = append(cpuList, strconv.Itoa(i))
	}

	return cpuList, nil
}
