// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cgmon

import (
	"context"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/pingcap/log"
	"github.com/pingcap/tidb/pkg/metrics"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/cgroup"
	"github.com/shirou/gopsutil/v3/mem"
	"go.uber.org/zap"
)

const (
	refreshInterval = 10 * time.Second
)

var (
	started         bool
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	lastCPU         int
	lastMemoryLimit uint64

	getCgroupCPUPeriodAndQuota = cgroup.GetCPUPeriodAndQuota
	getCgroupMemoryLimit       = cgroup.GetMemoryLimit
)

// StartCgroupMonitor uses to start the cgroup monitoring.
// WARN: this function is not thread-safe.
func StartCgroupMonitor() {
	if started {
		return
	}
	if runtime.GOOS != "linux" {
		return
	}
	started = true
	// Get configured maxprocs.
	ctx, cancel = context.WithCancel(context.Background())
	wg.Add(1)
	go refreshCgroupLoop()
	log.Info("cgroup monitor started")
}

// StopCgroupMonitor uses to stop the cgroup monitoring.
// WARN: this function is not thread-safe.
func StopCgroupMonitor() {
	if !started {
		return
	}
	if runtime.GOOS != "linux" {
		return
	}
	started = false
	if cancel != nil {
		cancel()
	}
	wg.Wait()
	log.Info("cgroup monitor stopped")
}

func refreshCgroupLoop() {
	ticker := time.NewTicker(refreshInterval)
	defer func() {
		wg.Done()
		ticker.Stop()
	}()
	defer util.Recover("cgmon", "refreshCgroupLoop", nil, false)

	err := refreshCgroupCPU()
	if err != nil {
		log.Warn("failed to get cgroup cpu quota", zap.Error(err))
	}
	err = refreshCgroupMemory()
	if err != nil {
		log.Warn("failed to get cgroup memory limit", zap.Error(err))
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err = refreshCgroupCPU()
			if err != nil {
				log.Debug("failed to get cgroup cpu quota", zap.Error(err))
			}
			err = refreshCgroupMemory()
			if err != nil {
				log.Debug("failed to get cgroup memory limit", zap.Error(err))
			}
		}
	}
}

func refreshCgroupCPU() error {
	// Get the number of CPUs.
	quota := runtime.NumCPU()

	// Get CPU quota from cgroup.
	cpuPeriod, cpuQuota, err := getCgroupCPUPeriodAndQuota()

	// Only use cgroup CPU quota if it is available. It's possible that the cgroup doesn't have CPU quota set.
	// Then in some environments, systemd will not enable the cpu controller if cpu quota is not set.
	if err == nil && cpuPeriod > 0 && cpuQuota > 0 {
		ratio := float64(cpuQuota) / float64(cpuPeriod)
		if ratio < float64(quota) {
			quota = int(math.Ceil(ratio))
		}
	}

	if quota != lastCPU {
		log.Info("set the maxprocs", zap.Int("quota", quota))
		metrics.MaxProcs.Set(float64(quota))
		lastCPU = quota
	}

	return err
}

func refreshCgroupMemory() error {
	// Get the total memory limit from `procfs`
	vmem, err := mem.VirtualMemory()
	if err != nil {
		return err
	}
	memLimit := vmem.Total

	// Only use cgroup memory limit if it is available.
	cgroupMemLimit, err := getCgroupMemoryLimit()
	if err == nil && cgroupMemLimit < memLimit {
		memLimit = cgroupMemLimit
	}

	if memLimit != lastMemoryLimit {
		log.Info("set the memory limit", zap.Uint64("memLimit", memLimit))
		metrics.MemoryLimit.Set(float64(memLimit))
		lastMemoryLimit = memLimit
	}
	return err
}
