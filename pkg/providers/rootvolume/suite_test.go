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

package rootvolume_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/samber/lo"
	clock "k8s.io/utils/clock/testing"

	"github.com/aws/karpenter-provider-aws/pkg/fake"
	"github.com/aws/karpenter-provider-aws/pkg/providers/rootvolume"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

const clusterName = "test-cluster"

var ctx context.Context
var stop context.CancelFunc
var fakeEC2API *fake.EC2API
var fakeClock *clock.FakeClock
var provider *rootvolume.DefaultProvider

func TestRootVolume(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "RootVolumeProvider")
}

var _ = BeforeSuite(func() {
	ctx, stop = context.WithCancel(ctx)
	fakeEC2API = fake.NewEC2API()
})

var _ = AfterSuite(func() {
	stop()
})

var _ = BeforeEach(func() {
	fakeEC2API.Reset()
	fakeClock = clock.NewFakeClock(time.Now())
	provider = rootvolume.NewDefaultProvider(ctx, fakeEC2API, fakeClock, clusterName)
	rootvolume.TasksTotal.Reset()
	rootvolume.TaskDurationSeconds.Reset()
})

// putTask stores a task in the fake EC2 API. token "" means a task Karpenter did not create.
func putTask(instanceID, imageID, token string, state ec2types.ReplaceRootVolumeTaskState, start time.Time) string {
	id := fake.ReplaceRootVolumeTaskID()
	task := ec2types.ReplaceRootVolumeTask{
		ReplaceRootVolumeTaskId: aws.String(id),
		InstanceId:              aws.String(instanceID),
		ImageId:                 aws.String(imageID),
		TaskState:               state,
		StartTime:               aws.String(start.UTC().Format(time.RFC3339)),
	}
	if token != "" {
		task.Tags = []ec2types.Tag{{Key: aws.String(rootvolume.TokenTagKey), Value: aws.String(token)}}
	}
	if state == ec2types.ReplaceRootVolumeTaskStateSucceeded || state == ec2types.ReplaceRootVolumeTaskStateFailed ||
		state == ec2types.ReplaceRootVolumeTaskStateFailedDetached {
		task.CompleteTime = aws.String(start.Add(5 * time.Minute).UTC().Format(time.RFC3339))
	}
	fakeEC2API.PutReplaceRootVolumeTask(task)
	return id
}

func describeCalls() int { return fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.Calls() }
func createCalls() int   { return fakeEC2API.CreateReplaceRootVolumeTaskBehavior.Calls() }

var _ = Describe("Token", func() {
	It("should be the 64 character hex SHA-256 of the operation ID", func() {
		operationID := "karpenter.sh/default/nodeclaim-abcde/reboot/0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0/2026-11-20T15:04:05Z"
		token := rootvolume.Token(operationID)
		Expect(token).To(HaveLen(64))
		Expect(token).To(MatchRegexp("^[0-9a-f]{64}$"))
		Expect(rootvolume.Token(operationID)).To(Equal(token))
		Expect(rootvolume.Token(operationID + "x")).ToNot(Equal(token))
		// SHA-256 of the empty string
		Expect(rootvolume.Token("")).To(Equal("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"))
	})
})

var _ = Describe("DryRun", func() {
	It("should ask with the same parameters as Create and return nil on DryRunOperation", func() {
		Expect(provider.DryRun(ctx, "i-dryrun", "ami-target")).To(Succeed())
		Expect(provider.Create(ctx, "i-real", "ami-target", rootvolume.Token("op"))).ToNot(BeNil())
		inputs := []*ec2.CreateReplaceRootVolumeTaskInput{}
		fakeEC2API.CreateReplaceRootVolumeTaskBehavior.CalledWithInput.ForEach(func(i *ec2.CreateReplaceRootVolumeTaskInput) { inputs = append(inputs, i) })
		Expect(inputs).To(HaveLen(2))
		dryRun, real := inputs[0], inputs[1]
		Expect(aws.ToBool(dryRun.DryRun)).To(BeTrue())
		Expect(aws.ToString(dryRun.ImageId)).To(Equal(aws.ToString(real.ImageId)))
		Expect(dryRun.DeleteReplacedRootVolume).To(Equal(real.DeleteReplacedRootVolume))
		Expect(dryRun.ClientToken).To(BeNil())
		Expect(dryRun.TagSpecifications).To(HaveLen(1))
		Expect(dryRun.TagSpecifications[0].ResourceType).To(Equal(real.TagSpecifications[0].ResourceType))
		Expect(lo.Map(dryRun.TagSpecifications[0].Tags, func(t ec2types.Tag, _ int) string { return aws.ToString(t.Key) })).
			To(Equal(lo.Map(real.TagSpecifications[0].Tags, func(t ec2types.Tag, _ int) string { return aws.ToString(t.Key) })))
		// No task is created by the dry run.
		Expect(provider.Latest(ctx, "i-dryrun")).To(BeNil())
	})
	It("should return UnauthorizedOperation when the role is missing a permission", func() {
		fakeEC2API.DenyReplaceRootVolumeTask("ec2:CreateTags")
		err := provider.DryRun(ctx, "i-dryrun", "ami-target")
		Expect(err).To(HaveOccurred())
		apiErr, ok := lo.ErrorsAs[smithy.APIError](err)
		Expect(ok).To(BeTrue())
		Expect(apiErr.ErrorCode()).To(Equal("UnauthorizedOperation"))
		Expect(apiErr.ErrorMessage()).To(ContainSubstring("ec2:CreateTags"))
	})
	It("should return other errors as they are", func() {
		fakeEC2API.CreateReplaceRootVolumeTaskBehavior.Error.Set(&smithy.GenericAPIError{Code: "RequestLimitExceeded"})
		err := provider.DryRun(ctx, "i-dryrun", "ami-target")
		apiErr, ok := lo.ErrorsAs[smithy.APIError](err)
		Expect(ok).To(BeTrue())
		Expect(apiErr.ErrorCode()).To(Equal("RequestLimitExceeded"))
	})
})

var _ = Describe("Create", func() {
	var instanceID, imageID, token string
	BeforeEach(func() {
		instanceID = fake.InstanceID()
		imageID = fake.ImageID()
		token = rootvolume.Token("operation-" + instanceID)
	})

	It("should create a task that deletes the old root volume, with the token as client token and tag", func() {
		task, err := provider.Create(ctx, instanceID, imageID, token)
		Expect(err).ToNot(HaveOccurred())
		Expect(task.ID).ToNot(BeEmpty())
		Expect(task.InstanceID).To(Equal(instanceID))
		Expect(task.ImageID).To(Equal(imageID))
		Expect(task.Token).To(Equal(token))
		Expect(task.Ours()).To(BeTrue())
		Expect(task.State).To(Equal(ec2types.ReplaceRootVolumeTaskStatePending))
		Expect(task.Terminal()).To(BeFalse())

		Expect(fakeEC2API.CreateReplaceRootVolumeTaskBehavior.CalledWithInput.Len()).To(Equal(1))
		input := fakeEC2API.CreateReplaceRootVolumeTaskBehavior.CalledWithInput.Pop()
		Expect(aws.ToString(input.InstanceId)).To(Equal(instanceID))
		Expect(aws.ToString(input.ImageId)).To(Equal(imageID))
		Expect(aws.ToBool(input.DeleteReplacedRootVolume)).To(BeTrue())
		Expect(aws.ToString(input.ClientToken)).To(Equal(token))
		Expect(input.SnapshotId).To(BeNil())
		Expect(input.VolumeId).To(BeNil())
		Expect(input.TagSpecifications).To(HaveLen(1))
		Expect(input.TagSpecifications[0].ResourceType).To(Equal(ec2types.ResourceTypeReplaceRootVolumeTask))
		tags := lo.SliceToMap(input.TagSpecifications[0].Tags, func(t ec2types.Tag) (string, string) {
			return aws.ToString(t.Key), aws.ToString(t.Value)
		})
		Expect(tags).To(Equal(map[string]string{
			rootvolume.TokenTagKey:                               token,
			fmt.Sprintf("kubernetes.io/cluster/%s", clusterName): "owned",
		}))
		// it looked for an existing task first
		Expect(describeCalls()).To(Equal(1))
	})
	DescribeTable("should adopt a task tagged with the token in any state without creating another",
		func(state ec2types.ReplaceRootVolumeTaskState) {
			id := putTask(instanceID, imageID, token, state, fakeClock.Now().Add(-time.Minute))
			task, err := provider.Create(ctx, instanceID, imageID, token)
			Expect(err).ToNot(HaveOccurred())
			Expect(task.ID).To(Equal(id))
			Expect(task.State).To(Equal(state))
			Expect(createCalls()).To(Equal(0))
		},
		Entry("pending", ec2types.ReplaceRootVolumeTaskStatePending),
		Entry("in-progress", ec2types.ReplaceRootVolumeTaskStateInProgress),
		Entry("failing", ec2types.ReplaceRootVolumeTaskStateFailing),
		Entry("failing-detached", ec2types.ReplaceRootVolumeTaskState("failing-detached")),
		Entry("succeeded", ec2types.ReplaceRootVolumeTaskStateSucceeded),
		Entry("failed", ec2types.ReplaceRootVolumeTaskStateFailed),
		Entry("failed-detached", ec2types.ReplaceRootVolumeTaskStateFailedDetached),
	)
	It("should not adopt a task with another token or no token", func() {
		putTask(instanceID, imageID, rootvolume.Token("another-operation"), ec2types.ReplaceRootVolumeTaskStateSucceeded, fakeClock.Now().Add(-time.Hour))
		putTask(instanceID, imageID, "", ec2types.ReplaceRootVolumeTaskStateFailed, fakeClock.Now().Add(-time.Hour))
		task, err := provider.Create(ctx, instanceID, imageID, token)
		Expect(err).ToNot(HaveOccurred())
		Expect(task.Token).To(Equal(token))
		Expect(createCalls()).To(Equal(1))
	})
	It("should create only once when called twice with the same token", func() {
		first, err := provider.Create(ctx, instanceID, imageID, token)
		Expect(err).ToNot(HaveOccurred())
		second, err := provider.Create(ctx, instanceID, imageID, token)
		Expect(err).ToNot(HaveOccurred())
		Expect(second.ID).To(Equal(first.ID))
		Expect(createCalls()).To(Equal(1))
	})
	It("should adopt its own task when EC2 reports an active task that the first describe missed", func() {
		id := putTask(instanceID, imageID, token, ec2types.ReplaceRootVolumeTaskStateInProgress, fakeClock.Now())
		// The first describe returns nothing, as if the task were not listed yet; the second reads the fake's state
		fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.MultiOut.Add(&ec2.DescribeReplaceRootVolumeTasksOutput{})
		task, err := provider.Create(ctx, instanceID, imageID, token)
		Expect(err).ToNot(HaveOccurred())
		Expect(task.ID).To(Equal(id))
		Expect(createCalls()).To(Equal(1))
		Expect(describeCalls()).To(Equal(2))
	})
	It("should return a TaskConflictError naming the active task when another task is running", func() {
		other := putTask(instanceID, fake.ImageID(), "", ec2types.ReplaceRootVolumeTaskStateInProgress, fakeClock.Now())
		_, err := provider.Create(ctx, instanceID, imageID, token)
		Expect(err).To(HaveOccurred())
		Expect(rootvolume.IsTaskConflictError(err)).To(BeTrue())
		conflict, ok := lo.ErrorsAs[*rootvolume.TaskConflictError](err)
		Expect(ok).To(BeTrue())
		Expect(conflict.InstanceID).To(Equal(instanceID))
		Expect(conflict.ActiveTaskID).To(Equal(other))
		Expect(describeCalls()).To(Equal(2))
	})
	It("should return a TaskConflictError without an active task for other incorrect instance states", func() {
		fakeEC2API.CreateReplaceRootVolumeTaskBehavior.Error.Set(&smithy.GenericAPIError{Code: "IncorrectInstanceState", Message: "The instance is stopped"})
		_, err := provider.Create(ctx, instanceID, imageID, token)
		Expect(rootvolume.IsTaskConflictError(err)).To(BeTrue())
		conflict, _ := lo.ErrorsAs[*rootvolume.TaskConflictError](err)
		Expect(conflict.ActiveTaskID).To(BeEmpty())
	})
	It("should return other create errors unchanged in kind", func() {
		fakeEC2API.CreateReplaceRootVolumeTaskBehavior.Error.Set(&smithy.GenericAPIError{Code: "InvalidAMIID.NotFound"})
		_, err := provider.Create(ctx, instanceID, imageID, token)
		Expect(err).To(HaveOccurred())
		Expect(rootvolume.IsTaskConflictError(err)).To(BeFalse())
		apiErr, ok := lo.ErrorsAs[smithy.APIError](err)
		Expect(ok).To(BeTrue())
		Expect(apiErr.ErrorCode()).To(Equal("InvalidAMIID.NotFound"))
		// only the adoption describe ran
		Expect(describeCalls()).To(Equal(1))
	})
	It("should not create when the adoption describe fails", func() {
		fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.Error.Set(&smithy.GenericAPIError{Code: "RequestLimitExceeded"})
		_, err := provider.Create(ctx, instanceID, imageID, token)
		Expect(err).To(HaveOccurred())
		Expect(createCalls()).To(Equal(0))
	})
	It("should reject an empty token", func() {
		_, err := provider.Create(ctx, instanceID, imageID, "")
		Expect(err).To(HaveOccurred())
		Expect(describeCalls()).To(Equal(0))
		Expect(createCalls()).To(Equal(0))
	})
	It("should cache the created task so Latest returns it before EC2 lists it", func() {
		created, err := provider.Create(ctx, instanceID, imageID, token)
		Expect(err).ToNot(HaveOccurred())
		fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.Output.Set(&ec2.DescribeReplaceRootVolumeTasksOutput{})
		latest, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(latest).ToNot(BeNil())
		Expect(latest.ID).To(Equal(created.ID))
	})
})

var _ = Describe("Latest", func() {
	var instanceID, imageID string
	BeforeEach(func() {
		instanceID = fake.InstanceID()
		imageID = fake.ImageID()
	})

	It("should return nil when the instance has no task", func() {
		task, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(task).To(BeNil())
	})
	It("should ignore tasks Karpenter did not create", func() {
		putTask(instanceID, imageID, "", ec2types.ReplaceRootVolumeTaskStateSucceeded, fakeClock.Now())
		task, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(task).To(BeNil())
	})
	It("should return Karpenter's task even when a newer untagged task exists", func() {
		ours := putTask(instanceID, imageID, rootvolume.Token("a"), ec2types.ReplaceRootVolumeTaskStateSucceeded, fakeClock.Now().Add(-time.Hour))
		putTask(instanceID, fake.ImageID(), "", ec2types.ReplaceRootVolumeTaskStateSucceeded, fakeClock.Now())
		task, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(task.ID).To(Equal(ours))
	})
	It("should return the task with the latest start time", func() {
		putTask(instanceID, imageID, rootvolume.Token("a"), ec2types.ReplaceRootVolumeTaskStateFailed, fakeClock.Now().Add(-2*time.Hour))
		latest := putTask(instanceID, imageID, rootvolume.Token("b"), ec2types.ReplaceRootVolumeTaskStateSucceeded, fakeClock.Now().Add(-time.Hour))
		putTask(instanceID, imageID, rootvolume.Token("c"), ec2types.ReplaceRootVolumeTaskStateFailed, fakeClock.Now().Add(-3*time.Hour))
		task, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(task.ID).To(Equal(latest))
	})
	It("should parse the space separated time format", func() {
		older := putTask(instanceID, imageID, rootvolume.Token("a"), ec2types.ReplaceRootVolumeTaskStateSucceeded, fakeClock.Now().Add(-time.Hour))
		newer := fake.ReplaceRootVolumeTaskID()
		fakeEC2API.PutReplaceRootVolumeTask(ec2types.ReplaceRootVolumeTask{
			ReplaceRootVolumeTaskId: aws.String(newer),
			InstanceId:              aws.String(instanceID),
			ImageId:                 aws.String(imageID),
			TaskState:               ec2types.ReplaceRootVolumeTaskStateSucceeded,
			StartTime:               aws.String(fakeClock.Now().UTC().Format("2006-01-02 15:04:05")),
			CompleteTime:            aws.String(fakeClock.Now().Add(time.Minute).UTC().Format("2006-01-02 15:04:05")),
			Tags:                    []ec2types.Tag{{Key: aws.String(rootvolume.TokenTagKey), Value: aws.String(rootvolume.Token("b"))}},
		})
		task, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(task.ID).To(Equal(newer))
		Expect(task.ID).ToNot(Equal(older))
		Expect(task.CompleteTime.Sub(task.StartTime)).To(Equal(time.Minute))
	})
	It("should serve a terminal task from the cache", func() {
		putTask(instanceID, imageID, rootvolume.Token("a"), ec2types.ReplaceRootVolumeTaskStateSucceeded, fakeClock.Now())
		first, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(describeCalls()).To(Equal(1))
		second, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(second).To(Equal(first))
		Expect(describeCalls()).To(Equal(1))
	})
	It("should re-read a non-terminal task until it finishes", func() {
		id := putTask(instanceID, imageID, rootvolume.Token("a"), ec2types.ReplaceRootVolumeTaskStateInProgress, fakeClock.Now())
		for i := 1; i <= 3; i++ {
			task, err := provider.Latest(ctx, instanceID)
			Expect(err).ToNot(HaveOccurred())
			Expect(task.State).To(Equal(ec2types.ReplaceRootVolumeTaskStateInProgress))
			Expect(describeCalls()).To(Equal(i))
		}
		fakeEC2API.SetReplaceRootVolumeTaskState(id, ec2types.ReplaceRootVolumeTaskStateSucceeded)
		task, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(task.Succeeded()).To(BeTrue())
		Expect(task.CompleteTime.IsZero()).To(BeFalse())
		_, err = provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(describeCalls()).To(Equal(4))
	})
	It("should treat unknown states as not terminal", func() {
		putTask(instanceID, imageID, rootvolume.Token("a"), ec2types.ReplaceRootVolumeTaskState("failing-detached"), fakeClock.Now())
		putTask(instanceID, imageID, rootvolume.Token("b"), ec2types.ReplaceRootVolumeTaskState("some-future-state"), fakeClock.Now().Add(-time.Minute))
		task, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(task.Terminal()).To(BeFalse())
		_, err = provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(describeCalls()).To(Equal(2))
	})
	It("should return describe errors", func() {
		fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.Error.Set(&smithy.GenericAPIError{Code: "RequestLimitExceeded"})
		task, err := provider.Latest(ctx, instanceID)
		Expect(err).To(HaveOccurred())
		Expect(task).To(BeNil())
	})
	It("should coalesce concurrent lookups for many instances into one describe", func() {
		instanceIDs := lo.Times(10, func(int) string { return fake.InstanceID() })
		for _, id := range instanceIDs {
			putTask(id, imageID, rootvolume.Token(id), ec2types.ReplaceRootVolumeTaskStateInProgress, fakeClock.Now())
		}
		var wg sync.WaitGroup
		for _, id := range instanceIDs {
			wg.Go(func() {
				defer GinkgoRecover()
				task, err := provider.Latest(ctx, id)
				Expect(err).ToNot(HaveOccurred())
				Expect(task.InstanceID).To(Equal(id))
			})
		}
		wg.Wait()
		Expect(describeCalls()).To(Equal(1))
	})
	It("should drop instances from the cache after they go unread for a day", func() {
		putTask(instanceID, imageID, rootvolume.Token("a"), ec2types.ReplaceRootVolumeTaskStateSucceeded, fakeClock.Now())
		_, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		fakeClock.Step(23 * time.Hour)
		_, err = provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(describeCalls()).To(Equal(1))
		// the read above refreshed the entry, so it lives another day from there
		fakeClock.Step(25 * time.Hour)
		task, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		Expect(task).ToNot(BeNil())
		Expect(describeCalls()).To(Equal(2))
	})
})

var _ = Describe("Failures", func() {
	var imageID string
	BeforeEach(func() {
		imageID = fake.ImageID()
	})
	hydrate := func(instanceID string) {
		GinkgoHelper()
		_, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
	}

	It("should count Karpenter's failed tasks toward the image that completed after since", func() {
		since := fakeClock.Now().Add(-time.Hour)
		cases := []struct {
			image string
			token string
			state ec2types.ReplaceRootVolumeTaskState
			start time.Time
		}{
			{imageID, "a", ec2types.ReplaceRootVolumeTaskStateFailed, fakeClock.Now().Add(-30 * time.Minute)},         // counted
			{imageID, "b", ec2types.ReplaceRootVolumeTaskStateFailedDetached, fakeClock.Now().Add(-20 * time.Minute)}, // counted
			{imageID, "c", ec2types.ReplaceRootVolumeTaskStateSucceeded, fakeClock.Now().Add(-20 * time.Minute)},      // succeeded
			{imageID, "d", ec2types.ReplaceRootVolumeTaskStateFailing, fakeClock.Now().Add(-20 * time.Minute)},        // not terminal
			{imageID, "e", ec2types.ReplaceRootVolumeTaskStateFailed, fakeClock.Now().Add(-3 * time.Hour)},            // too old
			{fake.ImageID(), "f", ec2types.ReplaceRootVolumeTaskStateFailed, fakeClock.Now().Add(-20 * time.Minute)},  // other image
			{imageID, "", ec2types.ReplaceRootVolumeTaskStateFailed, fakeClock.Now().Add(-20 * time.Minute)},          // not ours
		}
		for _, c := range cases {
			instanceID := fake.InstanceID()
			putTask(instanceID, c.image, lo.Ternary(c.token == "", "", rootvolume.Token(c.token)), c.state, c.start)
			hydrate(instanceID)
		}
		calls := describeCalls()
		Expect(provider.Failures(imageID, since)).To(Equal(2))
		Expect(provider.Failures(imageID, fakeClock.Now())).To(Equal(0))
		Expect(describeCalls()).To(Equal(calls))
	})
	It("should only read the cache", func() {
		putTask(fake.InstanceID(), imageID, rootvolume.Token("a"), ec2types.ReplaceRootVolumeTaskStateFailed, fakeClock.Now())
		Expect(provider.Failures(imageID, fakeClock.Now().Add(-time.Hour))).To(Equal(0))
		Expect(describeCalls()).To(Equal(0))
	})
})

var _ = Describe("Metrics", func() {
	var instanceID, imageID, token string
	BeforeEach(func() {
		instanceID = fake.InstanceID()
		imageID = fake.ImageID()
		token = rootvolume.Token("operation-" + instanceID)
	})

	It("should observe a task once when it finishes", func() {
		task, err := provider.Create(ctx, instanceID, imageID, token)
		Expect(err).ToNot(HaveOccurred())
		_, err = provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		_, found := FindMetricWithLabelValues("karpenter_aws_root_volume_replacement_tasks_total", map[string]string{"outcome": "succeeded"})
		Expect(found).To(BeFalse())

		fakeEC2API.SetReplaceRootVolumeTaskState(task.ID, ec2types.ReplaceRootVolumeTaskStateSucceeded)
		for range 3 {
			_, err = provider.Latest(ctx, instanceID)
			Expect(err).ToNot(HaveOccurred())
		}
		// a retried Create adopts the finished task and must not observe it again
		_, err = provider.Create(ctx, instanceID, imageID, token)
		Expect(err).ToNot(HaveOccurred())
		ExpectMetricCounterValue(rootvolume.TasksTotal, 1, map[string]string{"outcome": "succeeded"})
		ExpectMetricHistogramSampleCountValue("karpenter_aws_root_volume_replacement_task_duration_seconds", 1, map[string]string{"outcome": "succeeded"})
	})
	DescribeTable("should label the outcome",
		func(state ec2types.ReplaceRootVolumeTaskState, outcome string) {
			putTask(instanceID, imageID, token, state, fakeClock.Now().Add(-10*time.Minute))
			_, err := provider.Latest(ctx, instanceID)
			Expect(err).ToNot(HaveOccurred())
			ExpectMetricCounterValue(rootvolume.TasksTotal, 1, map[string]string{"outcome": outcome})
			metric, found := FindMetricWithLabelValues("karpenter_aws_root_volume_replacement_task_duration_seconds", map[string]string{"outcome": outcome})
			Expect(found).To(BeTrue())
			Expect(metric.Histogram.GetSampleCount()).To(BeNumerically("==", 1))
			Expect(metric.Histogram.GetSampleSum()).To(BeNumerically("==", (5 * time.Minute).Seconds()))
		},
		Entry("succeeded", ec2types.ReplaceRootVolumeTaskStateSucceeded, "succeeded"),
		Entry("failed", ec2types.ReplaceRootVolumeTaskStateFailed, "failed"),
		Entry("failed-detached", ec2types.ReplaceRootVolumeTaskStateFailedDetached, "failed_detached"),
	)
	It("should not observe tasks that finished long before the provider first saw them", func() {
		putTask(instanceID, imageID, token, ec2types.ReplaceRootVolumeTaskStateSucceeded, fakeClock.Now().Add(-3*time.Hour))
		_, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		_, found := FindMetricWithLabelValues("karpenter_aws_root_volume_replacement_tasks_total", map[string]string{"outcome": "succeeded"})
		Expect(found).To(BeFalse())
	})
	It("should not observe tasks Karpenter did not create", func() {
		putTask(instanceID, imageID, "", ec2types.ReplaceRootVolumeTaskStateSucceeded, fakeClock.Now())
		_, err := provider.Latest(ctx, instanceID)
		Expect(err).ToNot(HaveOccurred())
		_, found := FindMetricWithLabelValues("karpenter_aws_root_volume_replacement_tasks_total", map[string]string{"outcome": "succeeded"})
		Expect(found).To(BeFalse())
	})
})
