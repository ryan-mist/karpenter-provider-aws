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

package nodeclass_test

import (
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/awslabs/operatorpkg/object"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	coretest "sigs.k8s.io/karpenter/pkg/test"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/controllers/nodeclass"
	"github.com/aws/karpenter-provider-aws/pkg/fake"
	"github.com/aws/karpenter-provider-aws/pkg/operator/options"
	"github.com/aws/karpenter-provider-aws/pkg/providers/rootvolume"
	"github.com/aws/karpenter-provider-aws/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("NodeClass InPlaceUpgrade Status Controller", func() {
	var snapshotAPI *fake.SnapshotAPI
	var inPlaceController *nodeclass.Controller
	var amd64Image, arm64Image ec2types.Image

	image := func(id, arch, snapshotID string) ec2types.Image {
		return ec2types.Image{
			Name:           aws.String(fmt.Sprintf("amazon-eks-node-al2023-%s-standard-1.33-v20251101", arch)),
			ImageId:        aws.String(id),
			CreationDate:   aws.String(time.Now().Format(time.RFC3339)),
			Architecture:   ec2types.ArchitectureValues(arch),
			State:          ec2types.ImageStateAvailable,
			RootDeviceName: aws.String("/dev/xvda"),
			BlockDeviceMappings: []ec2types.BlockDeviceMapping{
				{DeviceName: aws.String("/dev/xvda"), Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String(snapshotID)}},
				{DeviceName: aws.String("/dev/xvdb"), Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String("snap-data")}},
			},
			Tags: []ec2types.Tag{{Key: aws.String("rvr"), Value: aws.String("true")}},
		}
	}
	setImages := func(images ...ec2types.Image) {
		awsEnv.EC2API.DescribeImagesOutput.Set(&ec2.DescribeImagesOutput{Images: images})
		awsEnv.AMICache.Flush()
		awsEnv.AMIProvider.ResetImages()
	}
	inPlaceUpgrade := func(id string) *v1.InPlaceUpgrade {
		nodeClass = ExpectExists(ctx, env.Client, nodeClass)
		ami, ok := lo.Find(nodeClass.Status.AMIs, func(a v1.AMI) bool { return a.ID == id })
		Expect(ok).To(BeTrue(), "ami %s isn't in status.amis", id)
		return withoutTime(ami.InPlaceUpgrade)
	}
	newController := func(opts ...nodeclass.Option) *nodeclass.Controller {
		return nodeclass.NewController(awsEnv.Clock, env.Client, cloudProvider, events.NewRecorder(&record.FakeRecorder{}), fake.DefaultRegion,
			awsEnv.SubnetProvider, awsEnv.SecurityGroupProvider, awsEnv.AMIProvider, awsEnv.InstanceProfileProvider, awsEnv.InstanceTypesProvider,
			awsEnv.LaunchTemplateProvider, awsEnv.CapacityReservationProvider, awsEnv.PlacementGroupProvider, awsEnv.EC2API, awsEnv.ValidationCache,
			awsEnv.RecreationCache, awsEnv.AMIResolver, awsEnv.CELEnvironment, options.FromContext(ctx).DisableDryRun, opts...)
	}

	BeforeEach(func() {
		snapshotAPI = fake.NewSnapshotAPI()
		inPlaceController = newController(nodeclass.WithSnapshotAPI(snapshotAPI))
		nodeClass = test.EC2NodeClass(v1.EC2NodeClass{
			Spec: v1.EC2NodeClassSpec{
				AMIFamily:        lo.ToPtr(v1.AMIFamilyAL2023),
				AMISelectorTerms: []v1.AMISelectorTerm{{Tags: map[string]string{"rvr": "true"}}},
				AMIDriftPolicy:   v1.AMIDriftPolicyReplaceRootVolume,
			},
		})
		amd64Image = image("ami-amd64", "x86_64", "snap-amd64")
		arm64Image = image("ami-arm64", "arm64", "snap-arm64")
		snapshotAPI.SetReadable("snap-amd64")
		snapshotAPI.SetReadable("snap-arm64")
		setImages(amd64Image, arm64Image)
	})

	It("should leave inPlaceUpgrade unset when amiDriftPolicy is ReplaceInstance", func() {
		nodeClass.Spec.AMIDriftPolicy = v1.AMIDriftPolicyReplaceInstance
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		nodeClass = ExpectExists(ctx, env.Client, nodeClass)
		Expect(nodeClass.Status.AMIs).To(HaveLen(2))
		for _, ami := range nodeClass.Status.AMIs {
			Expect(ami.InPlaceUpgrade).To(BeNil())
		}
		Expect(snapshotAPI.CallCount()).To(Equal(0))
	})
	It("should default amiDriftPolicy to ReplaceInstance", func() {
		nodeClass.Spec.AMIDriftPolicy = ""
		ExpectApplied(ctx, env.Client, nodeClass)
		nodeClass = ExpectExists(ctx, env.Client, nodeClass)
		Expect(nodeClass.Spec.AMIDriftPolicy).To(Equal(v1.AMIDriftPolicyReplaceInstance))
	})
	It("should clear inPlaceUpgrade when amiDriftPolicy is switched back to ReplaceInstance", func() {
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-amd64")).ToNot(BeNil())
		nodeClass = ExpectExists(ctx, env.Client, nodeClass)
		nodeClass.Spec.AMIDriftPolicy = v1.AMIDriftPolicyReplaceInstance
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-amd64")).To(BeNil())
		Expect(inPlaceUpgrade("ami-arm64")).To(BeNil())
	})
	It("should mark AMIs with readable root snapshots as supported", func() {
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
		Expect(inPlaceUpgrade("ami-arm64")).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
		Expect(snapshotAPI.Calls).To(HaveLen(1))
		input := snapshotAPI.Calls[0]
		// No restorable-by or owner values, so public snapshots are returned too, and no SnapshotIds, which would
		// fail the whole call when one snapshot isn't visible.
		Expect(input.RestorableByUserIds).To(BeEmpty())
		Expect(input.OwnerIds).To(BeEmpty())
		Expect(input.SnapshotIds).To(BeEmpty())
		Expect(input.Filters).To(HaveLen(1))
		Expect(aws.ToString(input.Filters[0].Name)).To(Equal("snapshot-id"))
		Expect(input.Filters[0].Values).To(ConsistOf("snap-amd64", "snap-arm64"))
	})
	It("should check the root device's snapshot, not other volumes in the AMI", func() {
		snapshotAPI.SetReadable("snap-data")
		delete(snapshotAPI.Snapshots, "snap-amd64")
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonSnapshotNotReadable}))
		Expect(inPlaceUpgrade("ami-arm64").Supported).To(BeTrue())
	})
	It("should mark AMIs whose root snapshot isn't shared with the account as SnapshotNotReadable", func() {
		delete(snapshotAPI.Snapshots, "snap-arm64")
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-arm64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonSnapshotNotReadable}))
		Expect(inPlaceUpgrade("ami-amd64").Supported).To(BeTrue())
	})
	DescribeTable("should check the root snapshot of every AMI, public or private",
		func(public bool, visibility string, expected *v1.InPlaceUpgrade) {
			arm64Image.Public = aws.Bool(public)
			setImages(amd64Image, arm64Image)
			switch visibility {
			case "public":
				snapshotAPI.SetPublic("snap-arm64")
			case "shared":
				snapshotAPI.SetReadable("snap-arm64")
			default:
				delete(snapshotAPI.Snapshots, "snap-arm64")
			}
			ExpectApplied(ctx, env.Client, nodeClass)
			ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
			Expect(inPlaceUpgrade("ami-arm64")).To(Equal(expected))
			Expect(inPlaceUpgrade("ami-amd64").Supported).To(BeTrue())
			Expect(snapshotAPI.Calls).To(HaveLen(1))
			Expect(snapshotAPI.Calls[0].Filters[0].Values).To(ContainElement("snap-arm64"))
		},
		Entry("public AMI with a public snapshot", true, "public", &v1.InPlaceUpgrade{Supported: true}),
		Entry("public AMI with a snapshot that isn't shared", true, "unshared", &v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonSnapshotNotReadable}),
		Entry("private AMI with a public snapshot", false, "public", &v1.InPlaceUpgrade{Supported: true}),
		Entry("private AMI with a shared snapshot", false, "shared", &v1.InPlaceUpgrade{Supported: true}),
		Entry("private AMI with a snapshot that isn't shared", false, "unshared", &v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonSnapshotNotReadable}),
	)
	It("should mark AMIs without an EBS root snapshot as SnapshotNotReadable", func() {
		amd64Image.RootDeviceName = aws.String("/dev/sda1")
		setImages(amd64Image, arm64Image)
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonSnapshotNotReadable}))
	})
	It("should mark AMIs with Marketplace product codes as ProductCodes", func() {
		amd64Image.ProductCodes = []ec2types.ProductCode{{ProductCodeId: aws.String("abc123"), ProductCodeType: ec2types.ProductCodeValuesMarketplace}}
		setImages(amd64Image, arm64Image)
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonProductCodes}))
		Expect(inPlaceUpgrade("ami-arm64").Supported).To(BeTrue())
	})
	It("should leave NitroTPM AMIs supported, since the NitroTPM rule depends on the instance", func() {
		arm64Image.TpmSupport = ec2types.TpmSupportValuesV20
		setImages(amd64Image, arm64Image)
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-arm64")).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
		Expect(inPlaceUpgrade("ami-amd64").Supported).To(BeTrue())
	})
	It("should mark Windows platform AMIs as UnsupportedAMIFamily", func() {
		amd64Image.Platform = ec2types.PlatformValuesWindows
		setImages(amd64Image, arm64Image)
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnsupportedAMIFamily}))
	})
	DescribeTable("should mark every AMI of an unsupported AMI family as UnsupportedAMIFamily",
		func(family string) {
			nodeClass.Spec.AMIFamily = lo.ToPtr(family)
			ExpectApplied(ctx, env.Client, nodeClass)
			ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
			Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnsupportedAMIFamily}))
			Expect(inPlaceUpgrade("ami-arm64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnsupportedAMIFamily}))
			Expect(snapshotAPI.CallCount()).To(Equal(0))
		},
		Entry("Custom", v1.AMIFamilyCustom),
		Entry("Windows2022", v1.AMIFamilyWindows2022),
	)
	It("should report Unknown and keep discovering AMIs when DescribeSnapshots fails", func() {
		snapshotAPI.Error = fmt.Errorf("throttled")
		ExpectApplied(ctx, env.Client, nodeClass)
		result := ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		nodeClass = ExpectExists(ctx, env.Client, nodeClass)
		Expect(nodeClass.Status.AMIs).To(HaveLen(2))
		Expect(nodeClass.StatusConditions().Get(v1.ConditionTypeAMIsReady).IsTrue()).To(BeTrue())
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnknown, Message: "checking the root snapshot's permissions failed"}))
		Expect(inPlaceUpgrade("ami-arm64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnknown, Message: "checking the root snapshot's permissions failed"}))
	})
	It("should report Unknown and keep discovering AMIs when the image attributes can't be described", func() {
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		// The AMI list stays cached while the image attribute lookup fails.
		awsEnv.AMIProvider.ResetImages()
		awsEnv.EC2API.NextError.Set(fmt.Errorf("throttled"))
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		nodeClass = ExpectExists(ctx, env.Client, nodeClass)
		Expect(nodeClass.Status.AMIs).To(HaveLen(2))
		Expect(nodeClass.StatusConditions().Get(v1.ConditionTypeAMIsReady).IsTrue()).To(BeTrue())
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnknown, Message: "describing the AMI failed"}))
	})
	It("should report Unknown when no snapshot API is configured", func() {
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, newController(), nodeClass)
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnknown, Message: "checking the root snapshot's permissions failed"}))
	})
	It("should cache snapshot results across reconciles", func() {
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		calls := snapshotAPI.CallCount()
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(snapshotAPI.CallCount()).To(Equal(calls))
	})
	It("should not patch the EC2NodeClass status when the result doesn't change", func() {
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		nodeClass = ExpectExists(ctx, env.Client, nodeClass)
		resourceVersion := nodeClass.ResourceVersion
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(ExpectExists(ctx, env.Client, nodeClass).ResourceVersion).To(Equal(resourceVersion))
	})
	It("should keep inPlaceUpgrade across the AMI reconciler rebuilding status.amis", func() {
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		nodeClass = ExpectExists(ctx, env.Client, nodeClass)
		// A new arm64 image replaces the old one; the amd64 entry keeps its result through the rebuild.
		newArm64 := image("ami-arm64-new", "arm64", "snap-arm64")
		newArm64.CreationDate = aws.String(time.Now().Add(time.Hour).Format(time.RFC3339))
		setImages(amd64Image, arm64Image, newArm64)
		_, err := nodeclass.NewAMIReconciler(awsEnv.Clock, awsEnv.AMIProvider).Reconcile(ctx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		amd64, ok := lo.Find(nodeClass.Status.AMIs, func(a v1.AMI) bool { return a.ID == "ami-amd64" })
		Expect(ok).To(BeTrue())
		Expect(withoutTime(amd64.InPlaceUpgrade)).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
		arm64, ok := lo.Find(nodeClass.Status.AMIs, func(a v1.AMI) bool { return a.ID == "ami-arm64-new" })
		Expect(ok).To(BeTrue())
		Expect(arm64.InPlaceUpgrade).To(BeNil())
		Expect(lo.ContainsBy(nodeClass.Status.AMIs, func(a v1.AMI) bool { return a.ID == "ami-arm64" })).To(BeFalse())
		// The full controller then fills in the new entry.
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-arm64-new").Supported).To(BeTrue())
	})
	It("should run right after the AMI reconciler so new AMIs are evaluated in the same reconcile", func() {
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		nodeClass = ExpectExists(ctx, env.Client, nodeClass)
		for _, ami := range nodeClass.Status.AMIs {
			Expect(ami.InPlaceUpgrade).ToNot(BeNil())
		}
		Expect(nodeClass.StatusConditions().Get(v1.ConditionTypeAMIsReady).IsTrue()).To(BeTrue())
	})
	It("should set the result on every status.amis entry for an AMI with several requirement sets", func() {
		ExpectApplied(ctx, env.Client, nodeClass)
		nodeClass.Status.AMIs = []v1.AMI{
			{ID: "ami-amd64", Requirements: []corev1.NodeSelectorRequirement{{Key: v1.LabelInstanceGPUCount, Operator: corev1.NodeSelectorOpExists}}},
			{ID: "ami-amd64", Requirements: []corev1.NodeSelectorRequirement{{Key: v1.LabelInstanceAcceleratorCount, Operator: corev1.NodeSelectorOpExists}}},
		}
		_, err := nodeclass.NewInPlaceUpgradeReconciler(awsEnv.Clock, env.Client, awsEnv.AMIProvider, snapshotAPI, nil).Reconcile(ctx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		Expect(withoutTime(nodeClass.Status.AMIs[0].InPlaceUpgrade)).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
		Expect(withoutTime(nodeClass.Status.AMIs[1].InPlaceUpgrade)).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
	})

	Context("Permission dry run", func() {
		var rootVolumes *rootvolume.DefaultProvider
		var dryRunController *nodeclass.Controller
		var nodeClaim *karpv1.NodeClaim

		nodeClaimFor := func(instanceID string) *karpv1.NodeClaim {
			nc := coretest.NodeClaim(karpv1.NodeClaim{
				Spec: karpv1.NodeClaimSpec{
					NodeClassRef: &karpv1.NodeClassReference{
						Group: object.GVK(nodeClass).Group,
						Kind:  object.GVK(nodeClass).Kind,
						Name:  nodeClass.Name,
					},
				},
				Status: karpv1.NodeClaimStatus{ProviderID: fake.ProviderID(instanceID)},
			})
			nc.StatusConditions().SetTrue(karpv1.ConditionTypeInitialized)
			return nc
		}
		dryRuns := func() []*ec2.CreateReplaceRootVolumeTaskInput {
			var out []*ec2.CreateReplaceRootVolumeTaskInput
			awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.CalledWithInput.ForEach(func(input *ec2.CreateReplaceRootVolumeTaskInput) {
				out = append(out, input)
			})
			return out
		}

		BeforeEach(func() {
			rootVolumes = rootvolume.NewDefaultProvider(ctx, awsEnv.EC2API, awsEnv.Clock, options.FromContext(ctx).ClusterName)
			dryRunController = newController(nodeclass.WithSnapshotAPI(snapshotAPI), nodeclass.WithRootVolumeProvider(rootVolumes))
			ExpectApplied(ctx, env.Client, nodeClass)
			nodeClaim = nodeClaimFor("i-0123456789abcdef0")
			ExpectApplied(ctx, env.Client, nodeClaim)
		})

		It("should mark AMIs supported when the dry run is allowed, with the same parameters as the real call", func() {
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
			Expect(inPlaceUpgrade("ami-arm64")).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
			inputs := dryRuns()
			Expect(inputs).To(HaveLen(2))
			Expect(lo.Map(inputs, func(i *ec2.CreateReplaceRootVolumeTaskInput, _ int) string { return aws.ToString(i.ImageId) })).To(ConsistOf("ami-amd64", "ami-arm64"))
			for _, input := range inputs {
				Expect(aws.ToBool(input.DryRun)).To(BeTrue())
				Expect(aws.ToString(input.InstanceId)).To(Equal("i-0123456789abcdef0"))
				Expect(aws.ToBool(input.DeleteReplacedRootVolume)).To(BeTrue())
				Expect(input.ClientToken).To(BeNil())
				Expect(input.TagSpecifications).To(HaveLen(1))
				Expect(input.TagSpecifications[0].ResourceType).To(Equal(ec2types.ResourceTypeReplaceRootVolumeTask))
				tags := lo.SliceToMap(input.TagSpecifications[0].Tags, func(t ec2types.Tag) (string, string) { return aws.ToString(t.Key), aws.ToString(t.Value) })
				Expect(tags).To(HaveKeyWithValue(fmt.Sprintf("kubernetes.io/cluster/%s", options.FromContext(ctx).ClusterName), "owned"))
				Expect(tags[rootvolume.TokenTagKey]).To(HaveLen(64))
			}
			// The dry run never creates a task.
			Expect(awsEnv.EC2API.DescribeReplaceRootVolumeTasks(ctx, &ec2.DescribeReplaceRootVolumeTasksInput{})).To(HaveField("ReplaceRootVolumeTasks", BeEmpty()))
		})
		DescribeTable("should mark AMIs PermissionDenied and name the denied action",
			func(action string) {
				awsEnv.EC2API.DenyReplaceRootVolumeTask(action)
				ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
				expected := &v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonPermissionDenied, Message: fmt.Sprintf("not authorized to perform %s", action)}
				Expect(inPlaceUpgrade("ami-amd64")).To(Equal(expected))
				Expect(inPlaceUpgrade("ami-arm64")).To(Equal(expected))
			},
			Entry("the task itself", "ec2:CreateReplaceRootVolumeTask"),
			Entry("tagging the task", "ec2:CreateTags"),
		)
		It("should fall back to CreateReplaceRootVolumeTask when the denial doesn't name an action", func() {
			awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.Error.Set(&smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "You are not authorized to perform this operation."}, fake.MaxCalls(0))
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonPermissionDenied, Message: "not authorized to perform ec2:CreateReplaceRootVolumeTask"}))
		})
		DescribeTable("should report Unknown and retry for transient dry run errors",
			func(err error) {
				awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.Error.Set(err, fake.MaxCalls(0))
				result := ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
				Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
				expected := &v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnknown, Message: "the CreateReplaceRootVolumeTask dry run failed"}
				Expect(inPlaceUpgrade("ami-amd64")).To(Equal(expected))
				Expect(inPlaceUpgrade("ami-arm64")).To(Equal(expected))
				// Transient results aren't cached, so the next reconcile tries again.
				awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.Error.Reset()
				ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
				Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
			},
			Entry("throttling", &smithy.GenericAPIError{Code: "RequestLimitExceeded", Message: "Request limit exceeded."}),
			Entry("a task already running on the instance", &smithy.GenericAPIError{Code: "IncorrectInstanceState", Message: "There is already a replace root volume task"}),
			Entry("a server error", &smithy.GenericAPIError{Code: "InternalError", Message: "An internal error has occurred", Fault: smithy.FaultServer}),
		)
		It("should cache the result with the other AMI checks and recompute it after the cache period", func() {
			awsEnv.EC2API.DenyReplaceRootVolumeTask("ec2:CreateReplaceRootVolumeTask")
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			Expect(inPlaceUpgrade("ami-amd64").Reason).To(Equal(v1.InPlaceUpgradeReasonPermissionDenied))
			calls := awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.Calls()
			// The role is fixed, but the cached denial holds until it expires.
			awsEnv.EC2API.DenyReplaceRootVolumeTask("")
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			Expect(awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.Calls()).To(Equal(calls))
			Expect(inPlaceUpgrade("ami-amd64").Reason).To(Equal(v1.InPlaceUpgradeReasonPermissionDenied))
			awsEnv.Clock.Step(5 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			Expect(awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.Calls()).To(Equal(calls + 2))
			Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
		})
		It("should run the dry run again when a drift reboot's task was rejected after the cached result", func() {
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			Expect(inPlaceUpgrade("ami-amd64").Supported).To(BeTrue())
			calls := awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.Calls()
			// The role lost the permission, and a drift reboot found out first.
			awsEnv.EC2API.DenyReplaceRootVolumeTask("ec2:CreateReplaceRootVolumeTask")
			awsEnv.Clock.Step(time.Second)
			rootVolumes.RecordRejection("i-0123456789abcdef0", "ami-amd64", "UnauthorizedOperation")
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			// Only the AMI with the rejection is checked again; the other keeps its cached result.
			Expect(awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.Calls()).To(Equal(calls + 1))
			Expect(inPlaceUpgrade("ami-amd64").Reason).To(Equal(v1.InPlaceUpgradeReasonPermissionDenied))
			Expect(inPlaceUpgrade("ami-arm64").Supported).To(BeTrue())
			// The new result is cached again.
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			Expect(awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.Calls()).To(Equal(calls + 1))
		})
		It("should leave the check pending without calling EC2 when the EC2NodeClass has no running node", func() {
			ExpectDeleted(ctx, env.Client, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			expected := &v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnknown, Message: "waiting for a running node to check permissions against"}
			Expect(inPlaceUpgrade("ami-amd64")).To(Equal(expected))
			Expect(awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.Calls()).To(Equal(0))
			// Once a node runs, the next reconcile checks.
			ExpectApplied(ctx, env.Client, nodeClaimFor("i-0aaaaaaaaaaaaaaaa"))
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
		})
		It("should only pick initialized NodeClaims that aren't rebooting or deleting", func() {
			ExpectDeleted(ctx, env.Client, nodeClaim)
			uninitialized := nodeClaimFor("i-0bbbbbbbbbbbbbbbb")
			uninitialized.StatusConditions().SetFalse(karpv1.ConditionTypeInitialized, "NotReady", "NotReady")
			rebooting := nodeClaimFor("i-0cccccccccccccccc")
			rebooting.StatusConditions().SetTrue(karpv1.ConditionTypeRebooting)
			otherNodeClass := nodeClaimFor("i-0dddddddddddddddd")
			otherNodeClass.Spec.NodeClassRef.Name = "other"
			ExpectApplied(ctx, env.Client, uninitialized, rebooting, otherNodeClass)
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			Expect(inPlaceUpgrade("ami-amd64").Message).To(Equal("waiting for a running node to check permissions against"))
			Expect(awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.Calls()).To(Equal(0))
			ready := nodeClaimFor("i-0eeeeeeeeeeeeeeee")
			ExpectApplied(ctx, env.Client, ready)
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			Expect(lo.Map(dryRuns(), func(i *ec2.CreateReplaceRootVolumeTaskInput, _ int) string { return aws.ToString(i.InstanceId) })).To(HaveEach("i-0eeeeeeeeeeeeeeee"))
		})
		It("should not dry run AMIs that already failed another check", func() {
			amd64Image.ProductCodes = []ec2types.ProductCode{{ProductCodeId: aws.String("abc123"), ProductCodeType: ec2types.ProductCodeValuesMarketplace}}
			setImages(amd64Image, arm64Image)
			ExpectObjectReconciled(ctx, env.Client, dryRunController, nodeClass)
			Expect(inPlaceUpgrade("ami-amd64").Reason).To(Equal(v1.InPlaceUpgradeReasonProductCodes))
			Expect(lo.Map(dryRuns(), func(i *ec2.CreateReplaceRootVolumeTaskInput, _ int) string { return aws.ToString(i.ImageId) })).To(ConsistOf("ami-arm64"))
		})
		It("should skip the dry run when dry runs are disabled", func() {
			dryRunCtx := options.ToContext(ctx, test.Options(test.OptionsFields{DisableDryRun: lo.ToPtr(true)}))
			ExpectObjectReconciled(dryRunCtx, env.Client, dryRunController, nodeClass)
			Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
			Expect(awsEnv.EC2API.CreateReplaceRootVolumeTaskBehavior.Calls()).To(Equal(0))
		})
	})

	Context("LastTransitionTime", func() {
		lastTransition := func(id string) time.Time {
			GinkgoHelper()
			nodeClass = ExpectExists(ctx, env.Client, nodeClass)
			ami, ok := lo.Find(nodeClass.Status.AMIs, func(a v1.AMI) bool { return a.ID == id })
			Expect(ok).To(BeTrue())
			Expect(ami.InPlaceUpgrade).ToNot(BeNil())
			Expect(ami.InPlaceUpgrade.LastTransitionTime).ToNot(BeNil())
			return ami.InPlaceUpgrade.LastTransitionTime.Time
		}
		It("should record when the result was first reported", func() {
			ExpectApplied(ctx, env.Client, nodeClass)
			ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
			Expect(lastTransition("ami-amd64")).To(BeTemporally("~", awsEnv.Clock.Now(), time.Second))
		})
		It("should keep the time while the result is unchanged, including across a controller restart", func() {
			snapshotAPI.Error = fmt.Errorf("throttled")
			ExpectApplied(ctx, env.Client, nodeClass)
			ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
			first := lastTransition("ami-amd64")
			awsEnv.Clock.Step(10 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
			Expect(lastTransition("ami-amd64")).To(BeTemporally("==", first))
			// A new controller has no in-memory state; the time comes from the status.
			awsEnv.Clock.Step(10 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, newController(nodeclass.WithSnapshotAPI(snapshotAPI)), nodeClass)
			Expect(lastTransition("ami-amd64")).To(BeTemporally("==", first))
			Expect(inPlaceUpgrade("ami-amd64").Reason).To(Equal(v1.InPlaceUpgradeReasonUnknown))
		})
		It("should move the time when the result changes", func() {
			snapshotAPI.Error = fmt.Errorf("throttled")
			ExpectApplied(ctx, env.Client, nodeClass)
			ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
			first := lastTransition("ami-amd64")
			awsEnv.Clock.Step(20 * time.Minute)
			snapshotAPI.Error = nil
			ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
			Expect(inPlaceUpgrade("ami-amd64").Supported).To(BeTrue())
			Expect(lastTransition("ami-amd64")).To(BeTemporally("~", first.Add(20*time.Minute), time.Second))
		})
		It("should keep the time across the AMI reconciler rebuilding status.amis", func() {
			snapshotAPI.Error = fmt.Errorf("throttled")
			ExpectApplied(ctx, env.Client, nodeClass)
			ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
			first := lastTransition("ami-amd64")
			awsEnv.Clock.Step(5 * time.Minute)
			awsEnv.AMICache.Flush()
			_, err := nodeclass.NewAMIReconciler(awsEnv.Clock, awsEnv.AMIProvider).Reconcile(ctx, nodeClass)
			Expect(err).ToNot(HaveOccurred())
			ami, _ := lo.Find(nodeClass.Status.AMIs, func(a v1.AMI) bool { return a.ID == "ami-amd64" })
			Expect(ami.InPlaceUpgrade.LastTransitionTime.Time).To(BeTemporally("==", first))
		})
	})
})

func withoutTime(u *v1.InPlaceUpgrade) *v1.InPlaceUpgrade {
	if u == nil {
		return nil
	}
	out := u.DeepCopy()
	out.LastTransitionTime = nil
	return out
}
