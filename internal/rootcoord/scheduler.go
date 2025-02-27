// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rootcoord

import (
	"context"
	"sync"
	"time"

	"github.com/milvus-io/milvus/internal/log"

	"go.uber.org/atomic"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/tso"

	"github.com/milvus-io/milvus/internal/allocator"
)

type IScheduler interface {
	Start()
	Stop()
	AddTask(t task) error
	GetMinDdlTs() Timestamp
}

type scheduler struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	idAllocator  allocator.Interface
	tsoAllocator tso.Allocator

	taskChan chan task

	lock sync.Mutex

	minDdlTs atomic.Uint64
}

func newScheduler(ctx context.Context, idAllocator allocator.Interface, tsoAllocator tso.Allocator) *scheduler {
	ctx1, cancel := context.WithCancel(ctx)
	// TODO
	n := 1024 * 10
	return &scheduler{
		ctx:          ctx1,
		cancel:       cancel,
		idAllocator:  idAllocator,
		tsoAllocator: tsoAllocator,
		taskChan:     make(chan task, n),
		minDdlTs:     *atomic.NewUint64(0),
	}
}

func (s *scheduler) Start() {
	s.wg.Add(1)
	go s.taskLoop()
}

func (s *scheduler) Stop() {
	s.cancel()
	s.wg.Wait()
}

func (s *scheduler) execute(task task) {
	defer s.setMinDdlTs(task.GetTs()) // we should update ts, whatever task succeeds or not.
	if err := task.Prepare(task.GetCtx()); err != nil {
		task.NotifyDone(err)
		return
	}
	err := task.Execute(task.GetCtx())
	task.NotifyDone(err)
}

func (s *scheduler) taskLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(Params.ProxyCfg.TimeTickInterval.GetAsDuration(time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.updateLatestTsoAsMinDdlTs()
		case task := <-s.taskChan:
			s.execute(task)
		}
	}
}

func (s *scheduler) updateLatestTsoAsMinDdlTs() {
	if len(s.taskChan) > 0 {
		return
	}

	ts, err := s.tsoAllocator.GenerateTSO(1)
	if err != nil {
		log.Warn("failed to generate tso, ignore to update min ddl ts", zap.Error(err))
	} else {
		s.setMinDdlTs(ts)
	}
}

func (s *scheduler) setID(task task) error {
	id, err := s.idAllocator.AllocOne()
	if err != nil {
		return err
	}
	task.SetID(id)
	return nil
}

func (s *scheduler) setTs(task task) error {
	ts, err := s.tsoAllocator.GenerateTSO(1)
	if err != nil {
		return err
	}
	task.SetTs(ts)
	return nil
}

func (s *scheduler) enqueue(task task) {
	s.taskChan <- task
}

func (s *scheduler) AddTask(task task) error {
	// make sure that setting ts and enqueue is atomic.
	s.lock.Lock()
	defer s.lock.Unlock()

	if err := s.setID(task); err != nil {
		return err
	}
	if err := s.setTs(task); err != nil {
		return err
	}
	s.enqueue(task)
	return nil
}

func (s *scheduler) GetMinDdlTs() Timestamp {
	return s.minDdlTs.Load()
}

func (s *scheduler) setMinDdlTs(ts Timestamp) {
	s.minDdlTs.Store(ts)
}
