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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pallinder/go-randomdata"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/samber/lo"
)

// ReplaceRootVolumeTaskBehavior fakes EC2 replace root volume tasks. Created tasks start pending and only move when a
// test calls SetReplaceRootVolumeTaskState. Like EC2, a create is rejected with IncorrectInstanceState while another
// task on the instance has not finished, and a create that reuses a client token returns the original task.
type ReplaceRootVolumeTaskBehavior struct {
	CreateReplaceRootVolumeTaskBehavior    MockedFunction[ec2.CreateReplaceRootVolumeTaskInput, ec2.CreateReplaceRootVolumeTaskOutput]
	DescribeReplaceRootVolumeTasksBehavior MockedFunction[ec2.DescribeReplaceRootVolumeTasksInput, ec2.DescribeReplaceRootVolumeTasksOutput]

	mu sync.Mutex
	// replaceRootVolumeTasks holds every task by ID
	replaceRootVolumeTasks map[string]ec2types.ReplaceRootVolumeTask
	// replaceRootVolumeTaskTokens maps a client token to the task it created
	replaceRootVolumeTaskTokens map[string]string
}

func (b *ReplaceRootVolumeTaskBehavior) resetReplaceRootVolumeTasks() {
	b.CreateReplaceRootVolumeTaskBehavior.Reset()
	b.DescribeReplaceRootVolumeTasksBehavior.Reset()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.replaceRootVolumeTasks = nil
	b.replaceRootVolumeTaskTokens = nil
}

// ReplaceRootVolumeTaskID returns a random task ID in the EC2 format.
func ReplaceRootVolumeTaskID() string {
	return fmt.Sprintf("replacevol-%s", strings.ToLower(randomdata.Alphanumeric(17)))
}

// fakeReplaceRootVolumeTaskTerminal mirrors rootvolume.Task.Terminal without importing it.
func fakeReplaceRootVolumeTaskTerminal(state ec2types.ReplaceRootVolumeTaskState) bool {
	return state == ec2types.ReplaceRootVolumeTaskStateSucceeded || state == ec2types.ReplaceRootVolumeTaskStateFailed ||
		state == ec2types.ReplaceRootVolumeTaskStateFailedDetached
}

// PutReplaceRootVolumeTask stores a task as is, for example one created outside Karpenter or before a restart.
func (b *ReplaceRootVolumeTaskBehavior) PutReplaceRootVolumeTask(task ec2types.ReplaceRootVolumeTask) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.replaceRootVolumeTasks == nil {
		b.replaceRootVolumeTasks = map[string]ec2types.ReplaceRootVolumeTask{}
	}
	b.replaceRootVolumeTasks[aws.ToString(task.ReplaceRootVolumeTaskId)] = task
}

// GetReplaceRootVolumeTask returns the stored task with the given ID.
func (b *ReplaceRootVolumeTaskBehavior) GetReplaceRootVolumeTask(id string) (ec2types.ReplaceRootVolumeTask, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	task, ok := b.replaceRootVolumeTasks[id]
	return task, ok
}

// SetReplaceRootVolumeTaskState moves a stored task to state, setting CompleteTime when the state is terminal.
func (b *ReplaceRootVolumeTaskBehavior) SetReplaceRootVolumeTaskState(id string, state ec2types.ReplaceRootVolumeTaskState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	task, ok := b.replaceRootVolumeTasks[id]
	if !ok {
		panic(fmt.Sprintf("replace root volume task %s does not exist", id))
	}
	task.TaskState = state
	if fakeReplaceRootVolumeTaskTerminal(state) {
		task.CompleteTime = aws.String(time.Now().UTC().Format(time.RFC3339))
	}
	b.replaceRootVolumeTasks[id] = task
}

// CreateReplaceRootVolumeTask is defined on EC2API rather than the behavior so that it shadows the embedded sdk.EC2API.
func (e *EC2API) CreateReplaceRootVolumeTask(_ context.Context, input *ec2.CreateReplaceRootVolumeTaskInput, _ ...func(*ec2.Options)) (*ec2.CreateReplaceRootVolumeTaskOutput, error) {
	b := &e.ReplaceRootVolumeTaskBehavior
	return b.CreateReplaceRootVolumeTaskBehavior.Invoke(input, func(input *ec2.CreateReplaceRootVolumeTaskInput) (*ec2.CreateReplaceRootVolumeTaskOutput, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.replaceRootVolumeTasks == nil {
			b.replaceRootVolumeTasks = map[string]ec2types.ReplaceRootVolumeTask{}
		}
		if b.replaceRootVolumeTaskTokens == nil {
			b.replaceRootVolumeTaskTokens = map[string]string{}
		}
		if token := aws.ToString(input.ClientToken); token != "" {
			if id, ok := b.replaceRootVolumeTaskTokens[token]; ok {
				task := b.replaceRootVolumeTasks[id]
				return &ec2.CreateReplaceRootVolumeTaskOutput{ReplaceRootVolumeTask: &task}, nil
			}
		}
		for _, task := range b.replaceRootVolumeTasks {
			if aws.ToString(task.InstanceId) == aws.ToString(input.InstanceId) && !fakeReplaceRootVolumeTaskTerminal(task.TaskState) {
				return nil, &smithy.GenericAPIError{
					Code:    "IncorrectInstanceState",
					Message: fmt.Sprintf("There is already a replace root volume task for instance %s", aws.ToString(input.InstanceId)),
				}
			}
		}
		var tags []ec2types.Tag
		for _, spec := range input.TagSpecifications {
			if spec.ResourceType == ec2types.ResourceTypeReplaceRootVolumeTask {
				tags = append(tags, spec.Tags...)
			}
		}
		task := ec2types.ReplaceRootVolumeTask{
			ReplaceRootVolumeTaskId:  aws.String(ReplaceRootVolumeTaskID()),
			InstanceId:               input.InstanceId,
			ImageId:                  input.ImageId,
			SnapshotId:               input.SnapshotId,
			DeleteReplacedRootVolume: input.DeleteReplacedRootVolume,
			TaskState:                ec2types.ReplaceRootVolumeTaskStatePending,
			StartTime:                aws.String(time.Now().UTC().Format(time.RFC3339)),
			Tags:                     tags,
		}
		b.replaceRootVolumeTasks[*task.ReplaceRootVolumeTaskId] = task
		if token := aws.ToString(input.ClientToken); token != "" {
			b.replaceRootVolumeTaskTokens[token] = *task.ReplaceRootVolumeTaskId
		}
		return &ec2.CreateReplaceRootVolumeTaskOutput{ReplaceRootVolumeTask: &task}, nil
	})
}

// DescribeReplaceRootVolumeTasks supports the instance-id filter and task IDs, the same as EC2. It returns every match
// on one page, sorted by task ID so results are stable.
func (e *EC2API) DescribeReplaceRootVolumeTasks(_ context.Context, input *ec2.DescribeReplaceRootVolumeTasksInput, _ ...func(*ec2.Options)) (*ec2.DescribeReplaceRootVolumeTasksOutput, error) {
	b := &e.ReplaceRootVolumeTaskBehavior
	return b.DescribeReplaceRootVolumeTasksBehavior.Invoke(input, func(input *ec2.DescribeReplaceRootVolumeTasksInput) (*ec2.DescribeReplaceRootVolumeTasksOutput, error) {
		var instanceIDs []string
		for _, filter := range input.Filters {
			if aws.ToString(filter.Name) != "instance-id" {
				return nil, &smithy.GenericAPIError{Code: "InvalidParameterValue", Message: fmt.Sprintf("unsupported filter %s", aws.ToString(filter.Name))}
			}
			instanceIDs = append(instanceIDs, filter.Values...)
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		tasks := lo.Filter(lo.Values(b.replaceRootVolumeTasks), func(task ec2types.ReplaceRootVolumeTask, _ int) bool {
			if len(input.ReplaceRootVolumeTaskIds) > 0 && !slices.Contains(input.ReplaceRootVolumeTaskIds, aws.ToString(task.ReplaceRootVolumeTaskId)) {
				return false
			}
			return len(input.Filters) == 0 || slices.Contains(instanceIDs, aws.ToString(task.InstanceId))
		})
		sort.Slice(tasks, func(i, j int) bool {
			return aws.ToString(tasks[i].ReplaceRootVolumeTaskId) < aws.ToString(tasks[j].ReplaceRootVolumeTaskId)
		})
		return &ec2.DescribeReplaceRootVolumeTasksOutput{ReplaceRootVolumeTasks: tasks}, nil
	})
}
