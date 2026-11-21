/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/pkg/test"

	"github.com/aws/karpenter-provider-aws/pkg/providers/rootvolume"
)

var _ rootvolume.Provider = (*RootVolumeProvider)(nil)

// CreateRootVolumeTaskInput records a call to RootVolumeProvider.Create.
type CreateRootVolumeTaskInput struct {
	InstanceID string
	ImageID    string
	Token      string
}

// RootVolumeProvider is an in-memory rootvolume.Provider. Tasks are keyed by instance ID, and Create records a
// pending task tagged with its token.
type RootVolumeProvider struct {
	mu    sync.Mutex
	tasks map[string][]*rootvolume.Task

	CreateCalls []CreateRootVolumeTaskInput
	// CreateError, when set, is returned by Create instead of creating a task.
	CreateError error
	// LatestError, when set, is returned by Latest.
	LatestError error
}

func NewRootVolumeProvider() *RootVolumeProvider {
	return &RootVolumeProvider{tasks: map[string][]*rootvolume.Task{}}
}

func (p *RootVolumeProvider) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tasks = map[string][]*rootvolume.Task{}
	p.CreateCalls = nil
	p.CreateError = nil
	p.LatestError = nil
}

// AddTask stores a task on its instance as if EC2 returned it.
func (p *RootVolumeProvider) AddTask(task *rootvolume.Task) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tasks[task.InstanceID] = append(p.tasks[task.InstanceID], task)
}

func (p *RootVolumeProvider) Create(_ context.Context, instanceID, imageID, token string) (*rootvolume.Task, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.CreateCalls = append(p.CreateCalls, CreateRootVolumeTaskInput{InstanceID: instanceID, ImageID: imageID, Token: token})
	if p.CreateError != nil {
		return nil, p.CreateError
	}
	if task, ok := lo.Find(p.tasks[instanceID], func(t *rootvolume.Task) bool { return t.Token == token }); ok {
		return task, nil
	}
	task := &rootvolume.Task{
		ID:         fmt.Sprintf("replacevol-%s", test.RandomName()),
		InstanceID: instanceID,
		ImageID:    imageID,
		State:      ec2types.ReplaceRootVolumeTaskStatePending,
		StartTime:  time.Now(),
		Token:      token,
	}
	p.tasks[instanceID] = append(p.tasks[instanceID], task)
	return task, nil
}

func (p *RootVolumeProvider) Latest(_ context.Context, instanceID string) (*rootvolume.Task, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.LatestError != nil {
		return nil, p.LatestError
	}
	ours := lo.Filter(p.tasks[instanceID], func(t *rootvolume.Task, _ int) bool { return t.Ours() })
	if len(ours) == 0 {
		return nil, nil
	}
	return lo.MaxBy(ours, func(a, b *rootvolume.Task) bool { return a.StartTime.After(b.StartTime) }), nil
}

func (p *RootVolumeProvider) Failures(imageID string, since time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return lo.SumBy(lo.Values(p.tasks), func(tasks []*rootvolume.Task) int {
		return lo.CountBy(tasks, func(t *rootvolume.Task) bool {
			return t.Ours() && t.Failed() && t.ImageID == imageID && t.CompleteTime.After(since)
		})
	})
}
