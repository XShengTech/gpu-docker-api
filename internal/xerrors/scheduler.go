package xerrors

import (
	"github.com/pkg/errors"
)

const (
	gpuNotEnough  = "gpu not enough"
	portNotEnough = "port not enough"
	cpuNotEnough  = "cpu not enough"
)

func NewGpuNotEnoughError() error {
	return errors.New(gpuNotEnough)
}

func IsGpuNotEnoughError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Cause(err).Error() == gpuNotEnough
}

func NewPortNotEnoughError() error {
	return errors.New(portNotEnough)
}

func IsPortNotEnoughError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Cause(err).Error() == portNotEnough
}

func NewCpuNotEnoughError() error {
	return errors.New(cpuNotEnough)
}

func IsCpuNotEnoughError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Cause(err).Error() == cpuNotEnough
}
