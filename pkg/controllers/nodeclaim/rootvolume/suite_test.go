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
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloudproviderfake "sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	coretest "sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"

	"github.com/aws/karpenter-provider-aws/pkg/apis"
	"github.com/aws/karpenter-provider-aws/pkg/controllers/nodeclaim/rootvolume"
	"github.com/aws/karpenter-provider-aws/pkg/fake"
	rootvolumeprovider "github.com/aws/karpenter-provider-aws/pkg/providers/rootvolume"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var stop context.CancelFunc
var env *coretest.Environment
var fakeEC2API *fake.EC2API
var fakeClock *clock.FakeClock
var provider *rootvolumeprovider.DefaultProvider
var controller *rootvolume.Controller

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "RootVolumeController")
}

var _ = BeforeSuite(func() {
	ctx, stop = context.WithCancel(ctx)
	env = coretest.NewEnvironment(coretest.WithCRDs(apis.CRDs...), coretest.WithCRDs(v1alpha1.CRDs...))
	fakeEC2API = fake.NewEC2API()
})

var _ = AfterSuite(func() {
	stop()
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	fakeEC2API.Reset()
	fakeClock = clock.NewFakeClock(time.Now().Truncate(time.Second))
	// A fresh provider per spec so the task cache doesn't leak between specs
	provider = rootvolumeprovider.NewDefaultProvider(ctx, fakeEC2API, fakeClock, "test-cluster")
	controller = rootvolume.NewController(env.Client, corecloudproviderfake.NewCloudProvider(), fakeClock, provider)
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

// withConditions replaces the NodeClaim's conditions with Launched=True and, unless rebooting is nil, a Rebooting
// condition that transitioned at the given time.
func withConditions(nc *karpv1.NodeClaim, rebooting *metav1.ConditionStatus, reason string, transitioned time.Time) *karpv1.NodeClaim {
	nc.Status.Conditions = []status.Condition{{
		Type: karpv1.ConditionTypeLaunched, Status: metav1.ConditionTrue, Reason: karpv1.ConditionTypeLaunched,
		LastTransitionTime: metav1.NewTime(transitioned.Add(-time.Hour)), ObservedGeneration: nc.Generation,
	}}
	if rebooting != nil {
		nc.Status.Conditions = append(nc.Status.Conditions, status.Condition{
			Type: karpv1.ConditionTypeRebooting, Status: *rebooting, Reason: reason, Message: "",
			LastTransitionTime: metav1.NewTime(transitioned), ObservedGeneration: nc.Generation,
		})
	}
	return nc
}

var (
	conditionTrue    = lo.ToPtr(metav1.ConditionTrue)
	conditionFalse   = lo.ToPtr(metav1.ConditionFalse)
	conditionUnknown = lo.ToPtr(metav1.ConditionUnknown)
)

var _ = Describe("InScope", func() {
	var nodeClaim *karpv1.NodeClaim
	BeforeEach(func() {
		nodeClaim = coretest.NodeClaim(karpv1.NodeClaim{Status: karpv1.NodeClaimStatus{ProviderID: fake.RandomProviderID()}})
	})
	now := time.Now()

	It("should include a rebooting NodeClaim regardless of how long it has been rebooting", func() {
		Expect(rootvolume.InScope(withConditions(nodeClaim, conditionTrue, karpv1.RebootReasonIssued, now.Add(-24*time.Hour)), now)).To(BeTrue())
	})
	DescribeTable("should include a NodeClaim whose reboot finished within the window, for any reason",
		func(reason string) {
			Expect(rootvolume.InScope(withConditions(nodeClaim, conditionFalse, reason, now.Add(-29*time.Minute)), now)).To(BeTrue())
		},
		Entry("succeeded", karpv1.RebootReasonSucceeded),
		Entry("failed", karpv1.RebootReasonFailed),
		Entry("other", "SomethingElse"),
	)
	It("should exclude a NodeClaim whose reboot finished before the window", func() {
		Expect(rootvolume.InScope(withConditions(nodeClaim, conditionFalse, karpv1.RebootReasonSucceeded, now.Add(-31*time.Minute)), now)).To(BeFalse())
	})
	It("should exclude a NodeClaim that never rebooted", func() {
		Expect(rootvolume.InScope(withConditions(nodeClaim, nil, "", now), now)).To(BeFalse())
	})
	It("should exclude an Unknown Rebooting condition", func() {
		Expect(rootvolume.InScope(withConditions(nodeClaim, conditionUnknown, "", now), now)).To(BeFalse())
	})
	It("should exclude a NodeClaim that is not launched", func() {
		withConditions(nodeClaim, conditionTrue, karpv1.RebootReasonIssued, now)
		nodeClaim.Status.Conditions[0].Status = metav1.ConditionFalse
		Expect(rootvolume.InScope(nodeClaim, now)).To(BeFalse())
	})
	It("should exclude a NodeClaim without a provider ID", func() {
		withConditions(nodeClaim, conditionTrue, karpv1.RebootReasonIssued, now)
		nodeClaim.Status.ProviderID = ""
		Expect(rootvolume.InScope(nodeClaim, now)).To(BeFalse())
	})
	It("should exclude a deleting NodeClaim", func() {
		withConditions(nodeClaim, conditionTrue, karpv1.RebootReasonIssued, now)
		nodeClaim.DeletionTimestamp = lo.ToPtr(metav1.NewTime(now))
		Expect(rootvolume.InScope(nodeClaim, now)).To(BeFalse())
	})
})

var _ = Describe("Controller", func() {
	var instanceID, launchImageID, targetImageID, token string
	var nodeClaim *karpv1.NodeClaim

	putTask := func(imageID, token string, state ec2types.ReplaceRootVolumeTaskState) string {
		id := fake.ReplaceRootVolumeTaskID()
		task := ec2types.ReplaceRootVolumeTask{
			ReplaceRootVolumeTaskId: aws.String(id),
			InstanceId:              aws.String(instanceID),
			ImageId:                 lo.EmptyableToPtr(imageID),
			TaskState:               state,
			StartTime:               aws.String(fakeClock.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)),
		}
		if token != "" {
			task.Tags = []ec2types.Tag{{Key: aws.String(rootvolumeprovider.TokenTagKey), Value: aws.String(token)}}
		}
		fakeEC2API.PutReplaceRootVolumeTask(task)
		return id
	}
	// apply stores the NodeClaim with the given Rebooting state, since ExpectApplied's status write sets conditions
	apply := func(rebooting *metav1.ConditionStatus, reason string, transitioned time.Time) {
		GinkgoHelper()
		ExpectApplied(ctx, env.Client, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		stored := nodeClaim.DeepCopy()
		withConditions(nodeClaim, rebooting, reason, transitioned)
		nodeClaim.Status.ImageID = launchImageID
		Expect(env.Client.Status().Patch(ctx, nodeClaim, client.MergeFrom(stored))).To(Succeed())
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
	}

	BeforeEach(func() {
		instanceID = fake.InstanceID()
		launchImageID = fake.ImageID()
		targetImageID = fake.ImageID()
		token = rootvolumeprovider.Token("operation-" + instanceID)
		nodeClaim = coretest.NodeClaim(karpv1.NodeClaim{Status: karpv1.NodeClaimStatus{ProviderID: fake.ProviderID(instanceID)}})
	})
	AfterEach(func() {
		// The controller never reads the instance provider, whose cache would serve the launch image
		Expect(fakeEC2API.DescribeInstancesBehavior.Calls()).To(Equal(0))
	})

	It("should do nothing when the instance has no task", func() {
		apply(conditionTrue, karpv1.RebootReasonIssued, fakeClock.Now())
		result := ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		Expect(result.RequeueAfter).To(BeZero())
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Status.ImageID).To(Equal(launchImageID))
	})
	It("should requeue while the task runs and patch the image once it succeeds", func() {
		apply(conditionTrue, karpv1.RebootReasonIssued, fakeClock.Now())
		id := putTask(targetImageID, token, ec2types.ReplaceRootVolumeTaskStateInProgress)
		result := ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		Expect(result.RequeueAfter).To(Equal(rootvolume.PollInterval))
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Status.ImageID).To(Equal(launchImageID))

		fakeEC2API.SetReplaceRootVolumeTaskState(id, ec2types.ReplaceRootVolumeTaskStateSucceeded)
		result = ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		Expect(result.RequeueAfter).To(BeZero())
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Status.ImageID).To(Equal(targetImageID))
	})
	It("should keep requeueing an unfinished task after the reboot finished, within the window", func() {
		apply(conditionFalse, karpv1.RebootReasonSucceeded, fakeClock.Now())
		putTask(targetImageID, token, ec2types.ReplaceRootVolumeTaskStateInProgress)
		result := ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		Expect(result.RequeueAfter).To(Equal(rootvolume.PollInterval))
		// once the window closes, the requeue stops without another describe
		fakeClock.Step(rootvolume.Window)
		calls := fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.Calls()
		result = ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		Expect(result.RequeueAfter).To(BeZero())
		Expect(fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.Calls()).To(Equal(calls))
	})
	It("should patch the image for a reboot that finished within the window", func() {
		apply(conditionFalse, karpv1.RebootReasonSucceeded, fakeClock.Now().Add(-10*time.Minute))
		putTask(targetImageID, token, ec2types.ReplaceRootVolumeTaskStateSucceeded)
		ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Status.ImageID).To(Equal(targetImageID))
	})
	It("should patch only status.imageID", func() {
		apply(conditionTrue, karpv1.RebootReasonIssued, fakeClock.Now())
		putTask(targetImageID, token, ec2types.ReplaceRootVolumeTaskStateSucceeded)
		stale := nodeClaim.DeepCopy()
		// The reboot lifecycle moves on after the controller read the NodeClaim
		latest := ExpectExists(ctx, env.Client, nodeClaim)
		stored := latest.DeepCopy()
		latest.StatusConditions().SetFalse(karpv1.ConditionTypeRebooting, karpv1.RebootReasonSucceeded, "rebooted")
		latest.Status.NodeName = "renamed"
		Expect(env.Client.Status().Patch(ctx, latest, client.MergeFrom(stored))).To(Succeed())

		_, err := controller.Reconcile(ctx, stale)
		Expect(err).ToNot(HaveOccurred())
		result := ExpectExists(ctx, env.Client, nodeClaim)
		Expect(result.Status.ImageID).To(Equal(targetImageID))
		Expect(result.Status.NodeName).To(Equal("renamed"))
		Expect(result.StatusConditions().Get(karpv1.ConditionTypeRebooting).Reason).To(Equal(karpv1.RebootReasonSucceeded))
	})
	It("should not patch when the image already matches", func() {
		apply(conditionTrue, karpv1.RebootReasonIssued, fakeClock.Now())
		putTask(launchImageID, token, ec2types.ReplaceRootVolumeTaskStateSucceeded)
		resourceVersion := nodeClaim.ResourceVersion
		result := ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		Expect(result.RequeueAfter).To(BeZero())
		Expect(ExpectExists(ctx, env.Client, nodeClaim).ResourceVersion).To(Equal(resourceVersion))
	})
	It("should never write an empty image", func() {
		apply(conditionTrue, karpv1.RebootReasonIssued, fakeClock.Now())
		// a launch-state or snapshot task has no image
		putTask("", token, ec2types.ReplaceRootVolumeTaskStateSucceeded)
		resourceVersion := nodeClaim.ResourceVersion
		ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		latest := ExpectExists(ctx, env.Client, nodeClaim)
		Expect(latest.Status.ImageID).To(Equal(launchImageID))
		Expect(latest.ResourceVersion).To(Equal(resourceVersion))
	})
	DescribeTable("should not patch for a failed task",
		func(state ec2types.ReplaceRootVolumeTaskState) {
			apply(conditionFalse, karpv1.RebootReasonFailed, fakeClock.Now())
			putTask(targetImageID, token, state)
			result := ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
			Expect(result.RequeueAfter).To(BeZero())
			Expect(ExpectExists(ctx, env.Client, nodeClaim).Status.ImageID).To(Equal(launchImageID))
		},
		Entry("failed", ec2types.ReplaceRootVolumeTaskStateFailed),
		Entry("failed-detached", ec2types.ReplaceRootVolumeTaskStateFailedDetached),
	)
	It("should ignore tasks Karpenter did not create", func() {
		apply(conditionTrue, karpv1.RebootReasonIssued, fakeClock.Now())
		putTask(targetImageID, "", ec2types.ReplaceRootVolumeTaskStateSucceeded)
		result := ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		Expect(result.RequeueAfter).To(BeZero())
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Status.ImageID).To(Equal(launchImageID))
	})
	It("should not look up tasks for a NodeClaim out of scope", func() {
		apply(conditionFalse, karpv1.RebootReasonSucceeded, fakeClock.Now().Add(-time.Hour))
		putTask(targetImageID, token, ec2types.ReplaceRootVolumeTaskStateSucceeded)
		ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		Expect(fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.Calls()).To(Equal(0))
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Status.ImageID).To(Equal(launchImageID))
	})
	It("should return describe errors so the NodeClaim is retried", func() {
		apply(conditionTrue, karpv1.RebootReasonIssued, fakeClock.Now())
		fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.Error.Set(&smithy.GenericAPIError{Code: "RequestLimitExceeded"})
		Expect(ExpectObjectReconcileFailed(ctx, env.Client, controller, nodeClaim).Error()).To(ContainSubstring("RequestLimitExceeded"))
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Status.ImageID).To(Equal(launchImageID))
	})
	It("should ignore a malformed provider ID", func() {
		nodeClaim.Status.ProviderID = "malformed"
		apply(conditionTrue, karpv1.RebootReasonIssued, fakeClock.Now())
		result := ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		Expect(result.RequeueAfter).To(BeZero())
		Expect(fakeEC2API.DescribeReplaceRootVolumeTasksBehavior.Calls()).To(Equal(0))
	})
})
