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

package batcher

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/samber/lo"

	sdk "github.com/aws/karpenter-provider-aws/pkg/aws"
)

const (
	// replaceRootVolumeTasksInstanceIDFilter is the only filter DescribeReplaceRootVolumeTasks supports.
	replaceRootVolumeTasksInstanceIDFilter = "instance-id"
	// maxReplaceRootVolumeTasksFilterValues caps the instance-id values in one call. EC2 does not publish a limit for
	// this API; 100 stays well under the 200 values per filter that other EC2 describe APIs accept.
	maxReplaceRootVolumeTasksFilterValues = 100
)

type DescribeReplaceRootVolumeTasksBatcher struct {
	batcher *Batcher[ec2.DescribeReplaceRootVolumeTasksInput, ec2.DescribeReplaceRootVolumeTasksOutput]
}

func NewDescribeReplaceRootVolumeTasksBatcher(ctx context.Context, ec2api sdk.EC2API) *DescribeReplaceRootVolumeTasksBatcher {
	options := Options[ec2.DescribeReplaceRootVolumeTasksInput, ec2.DescribeReplaceRootVolumeTasksOutput]{
		Name:        "describe_replace_root_volume_tasks",
		IdleTimeout: 100 * time.Millisecond,
		MaxTimeout:  1 * time.Second,
		// MaxItems is a soft limit on the batch size, so the executor also chunks by maxReplaceRootVolumeTasksFilterValues
		MaxItems:      maxReplaceRootVolumeTasksFilterValues,
		RequestHasher: OneBucketHasher[ec2.DescribeReplaceRootVolumeTasksInput],
		BatchExecutor: execDescribeReplaceRootVolumeTasksBatch(ec2api),
	}
	return &DescribeReplaceRootVolumeTasksBatcher{batcher: NewBatcher(ctx, options)}
}

// DescribeReplaceRootVolumeTasks describes the tasks of a single instance. The input must carry exactly one instance-id
// filter with exactly one value and nothing else. Concurrent calls are coalesced into one paginated describe whose
// instance-id filter lists every instance; each caller gets back only its own instance's tasks.
func (b *DescribeReplaceRootVolumeTasksBatcher) DescribeReplaceRootVolumeTasks(ctx context.Context, input *ec2.DescribeReplaceRootVolumeTasksInput) (*ec2.DescribeReplaceRootVolumeTasksOutput, error) {
	if _, err := replaceRootVolumeTasksInstanceID(input); err != nil {
		return nil, err
	}
	result := b.batcher.Add(ctx, input)
	return result.Output, result.Err
}

func replaceRootVolumeTasksInstanceID(input *ec2.DescribeReplaceRootVolumeTasksInput) (string, error) {
	if len(input.ReplaceRootVolumeTaskIds) != 0 || input.NextToken != nil || input.MaxResults != nil || len(input.Filters) != 1 ||
		aws.ToString(input.Filters[0].Name) != replaceRootVolumeTasksInstanceIDFilter || len(input.Filters[0].Values) != 1 {
		return "", fmt.Errorf("expected a single instance-id filter with a single value")
	}
	return input.Filters[0].Values[0], nil
}

func execDescribeReplaceRootVolumeTasksBatch(ec2api sdk.EC2API) BatchExecutor[ec2.DescribeReplaceRootVolumeTasksInput, ec2.DescribeReplaceRootVolumeTasksOutput] {
	return func(ctx context.Context, inputs []*ec2.DescribeReplaceRootVolumeTasksInput) []Result[ec2.DescribeReplaceRootVolumeTasksOutput] {
		results := make([]Result[ec2.DescribeReplaceRootVolumeTasksOutput], len(inputs))
		instanceIDs := lo.Uniq(lo.Map(inputs, func(input *ec2.DescribeReplaceRootVolumeTasksInput, _ int) string {
			return lo.Must(replaceRootVolumeTasksInstanceID(input))
		}))
		tasksByInstance := map[string][]ec2types.ReplaceRootVolumeTask{}
		errsByInstance := map[string]error{}
		for _, chunk := range lo.Chunk(instanceIDs, maxReplaceRootVolumeTasksFilterValues) {
			if err := describeReplaceRootVolumeTasks(ctx, ec2api, chunk, tasksByInstance); err != nil {
				// A partial result would read as "this instance has no task", so every caller in the chunk gets the
				// error instead.
				for _, id := range chunk {
					errsByInstance[id] = err
				}
			}
		}
		for i, input := range inputs {
			id := lo.Must(replaceRootVolumeTasksInstanceID(input))
			if err, ok := errsByInstance[id]; ok {
				results[i] = Result[ec2.DescribeReplaceRootVolumeTasksOutput]{Err: err}
				continue
			}
			results[i] = Result[ec2.DescribeReplaceRootVolumeTasksOutput]{Output: &ec2.DescribeReplaceRootVolumeTasksOutput{
				ReplaceRootVolumeTasks: append([]ec2types.ReplaceRootVolumeTask{}, tasksByInstance[id]...),
			}}
		}
		return results
	}
}

// describeReplaceRootVolumeTasks pages through the tasks of instanceIDs into tasksByInstance. On error it leaves
// tasksByInstance untouched.
func describeReplaceRootVolumeTasks(ctx context.Context, ec2api sdk.EC2API, instanceIDs []string, tasksByInstance map[string][]ec2types.ReplaceRootVolumeTask) error {
	var tasks []ec2types.ReplaceRootVolumeTask
	paginator := ec2.NewDescribeReplaceRootVolumeTasksPaginator(ec2api, &ec2.DescribeReplaceRootVolumeTasksInput{
		Filters: []ec2types.Filter{{Name: aws.String(replaceRootVolumeTasksInstanceIDFilter), Values: instanceIDs}},
	})
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("describing replace root volume tasks, %w", err)
		}
		tasks = append(tasks, output.ReplaceRootVolumeTasks...)
	}
	for _, task := range tasks {
		id := aws.ToString(task.InstanceId)
		tasksByInstance[id] = append(tasksByInstance[id], task)
	}
	return nil
}
