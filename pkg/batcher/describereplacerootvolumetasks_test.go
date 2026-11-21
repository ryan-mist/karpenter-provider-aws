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

package batcher_test

import (
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/samber/lo"

	"github.com/aws/karpenter-provider-aws/pkg/batcher"
	"github.com/aws/karpenter-provider-aws/pkg/fake"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DescribeReplaceRootVolumeTasks Batcher", func() {
	var b *batcher.DescribeReplaceRootVolumeTasksBatcher

	byInstance := func(instanceID string) *ec2.DescribeReplaceRootVolumeTasksInput {
		return &ec2.DescribeReplaceRootVolumeTasksInput{
			Filters: []ec2types.Filter{{Name: aws.String("instance-id"), Values: []string{instanceID}}},
		}
	}
	task := func(instanceID string) ec2types.ReplaceRootVolumeTask {
		return ec2types.ReplaceRootVolumeTask{
			ReplaceRootVolumeTaskId: aws.String(fake.ReplaceRootVolumeTaskID()),
			InstanceId:              aws.String(instanceID),
			TaskState:               ec2types.ReplaceRootVolumeTaskStateInProgress,
		}
	}
	describeConcurrently := func(instanceIDs []string) []*ec2.DescribeReplaceRootVolumeTasksOutput {
		outputs := make([]*ec2.DescribeReplaceRootVolumeTasksOutput, len(instanceIDs))
		var wg sync.WaitGroup
		for i, id := range instanceIDs {
			wg.Go(func() {
				defer GinkgoRecover()
				out, err := b.DescribeReplaceRootVolumeTasks(ctx, byInstance(id))
				Expect(err).ToNot(HaveOccurred())
				outputs[i] = out
			})
		}
		wg.Wait()
		return outputs
	}

	BeforeEach(func() {
		fakeEC2API.Reset()
		b = batcher.NewDescribeReplaceRootVolumeTasksBatcher(ctx, fakeEC2API)
	})

	It("should coalesce concurrent lookups into one call with every instance in the instance-id filter", func() {
		instanceIDs := []string{"i-1", "i-2", "i-3", "i-4", "i-5"}
		for _, id := range instanceIDs {
			fakeEC2API.PutReplaceRootVolumeTask(task(id))
		}
		outputs := describeConcurrently(instanceIDs)
		for i, id := range instanceIDs {
			Expect(outputs[i].ReplaceRootVolumeTasks).To(HaveLen(1))
			Expect(aws.ToString(outputs[i].ReplaceRootVolumeTasks[0].InstanceId)).To(Equal(id))
		}
		Expect(fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.CalledWithInput.Len()).To(Equal(1))
		call := fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.CalledWithInput.Pop()
		Expect(call.ReplaceRootVolumeTaskIds).To(BeEmpty())
		Expect(call.Filters).To(HaveLen(1))
		Expect(aws.ToString(call.Filters[0].Name)).To(Equal("instance-id"))
		Expect(call.Filters[0].Values).To(ConsistOf(instanceIDs))
	})
	It("should dedupe repeated instances and give each caller its own tasks", func() {
		fakeEC2API.PutReplaceRootVolumeTask(task("i-1"))
		fakeEC2API.PutReplaceRootVolumeTask(task("i-1"))
		fakeEC2API.PutReplaceRootVolumeTask(task("i-2"))
		outputs := describeConcurrently([]string{"i-1", "i-1", "i-2", "i-3"})
		Expect(outputs[0].ReplaceRootVolumeTasks).To(HaveLen(2))
		Expect(outputs[1].ReplaceRootVolumeTasks).To(HaveLen(2))
		Expect(outputs[2].ReplaceRootVolumeTasks).To(HaveLen(1))
		Expect(outputs[3].ReplaceRootVolumeTasks).To(BeEmpty())
		Expect(fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.CalledWithInput.Len()).To(Equal(1))
		call := fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.CalledWithInput.Pop()
		Expect(call.Filters[0].Values).To(ConsistOf("i-1", "i-2", "i-3"))
	})
	It("should follow pagination and split every page by instance", func() {
		fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.OutputPages.Add(&ec2.DescribeReplaceRootVolumeTasksOutput{
			ReplaceRootVolumeTasks: []ec2types.ReplaceRootVolumeTask{task("i-1"), task("i-2")},
		})
		fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.OutputPages.Add(&ec2.DescribeReplaceRootVolumeTasksOutput{
			ReplaceRootVolumeTasks: []ec2types.ReplaceRootVolumeTask{task("i-1")},
		})
		outputs := describeConcurrently([]string{"i-1", "i-2"})
		Expect(outputs[0].ReplaceRootVolumeTasks).To(HaveLen(2))
		Expect(outputs[1].ReplaceRootVolumeTasks).To(HaveLen(1))
		Expect(fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.CalledWithInput.Len()).To(Equal(2))
	})
	It("should return the error to every caller when a page fails", func() {
		fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.Error.Set(&smithy.GenericAPIError{Code: "RequestLimitExceeded"})
		var wg sync.WaitGroup
		for _, id := range []string{"i-1", "i-2"} {
			wg.Go(func() {
				defer GinkgoRecover()
				out, err := b.DescribeReplaceRootVolumeTasks(ctx, byInstance(id))
				Expect(err).To(HaveOccurred())
				Expect(out).To(BeNil())
			})
		}
		wg.Wait()
	})
	DescribeTable("should reject inputs that are not a single instance lookup",
		func(input *ec2.DescribeReplaceRootVolumeTasksInput) {
			_, err := b.DescribeReplaceRootVolumeTasks(ctx, input)
			Expect(err).To(HaveOccurred())
			Expect(fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.Calls()).To(Equal(0))
		},
		Entry("no filter", &ec2.DescribeReplaceRootVolumeTasksInput{}),
		Entry("two values", &ec2.DescribeReplaceRootVolumeTasksInput{
			Filters: []ec2types.Filter{{Name: aws.String("instance-id"), Values: []string{"i-1", "i-2"}}},
		}),
		Entry("task IDs", &ec2.DescribeReplaceRootVolumeTasksInput{
			Filters:                  []ec2types.Filter{{Name: aws.String("instance-id"), Values: []string{"i-1"}}},
			ReplaceRootVolumeTaskIds: []string{"replacevol-1"},
		}),
		Entry("another filter", &ec2.DescribeReplaceRootVolumeTasksInput{
			Filters: []ec2types.Filter{{Name: aws.String("task-state"), Values: []string{"pending"}}},
		}),
	)
	It("should batch more instances than fit in one call across several calls", func() {
		instanceIDs := lo.Times(150, func(i int) string { return fmt.Sprintf("i-%d", i) })
		for _, id := range instanceIDs {
			fakeEC2API.PutReplaceRootVolumeTask(task(id))
		}
		outputs := describeConcurrently(instanceIDs)
		for i := range instanceIDs {
			Expect(outputs[i].ReplaceRootVolumeTasks).To(HaveLen(1))
		}
		Expect(fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.CalledWithInput.Len()).To(BeNumerically(">=", 2))
		fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.CalledWithInput.ForEach(func(call *ec2.DescribeReplaceRootVolumeTasksInput) {
			Expect(len(call.Filters[0].Values)).To(BeNumerically("<=", 100))
		})
	})
})
