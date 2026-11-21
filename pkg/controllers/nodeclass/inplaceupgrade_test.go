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
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/karpenter/pkg/events"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/controllers/nodeclass"
	"github.com/aws/karpenter-provider-aws/pkg/fake"
	"github.com/aws/karpenter-provider-aws/pkg/operator/options"
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
		return ami.InPlaceUpgrade
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
		input := snapshotAPI.Calls[0]
		Expect(input.RestorableByUserIds).To(Equal([]string{"self"}))
		Expect(input.Filters).To(HaveLen(1))
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
	It("should accept a public snapshot", func() {
		snapshotAPI.Snapshots["snap-arm64"] = []string{"all"}
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-arm64").Supported).To(BeTrue())
		Expect(snapshotAPI.Calls).To(HaveLen(2))
		Expect(snapshotAPI.Calls[1].RestorableByUserIds).To(Equal([]string{"all"}))
		Expect(snapshotAPI.Calls[1].Filters[0].Values).To(ConsistOf("snap-arm64"))
	})
	It("should mark AMIs without an EBS root snapshot as SnapshotNotReadable", func() {
		amd64Image.RootDeviceName = aws.String("/dev/sda1")
		setImages(amd64Image, arm64Image)
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonSnapshotNotReadable}))
	})
	It("should skip the snapshot check for public AMIs", func() {
		amd64Image.Public = aws.Bool(true)
		arm64Image.Public = aws.Bool(true)
		snapshotAPI.Reset()
		setImages(amd64Image, arm64Image)
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-amd64").Supported).To(BeTrue())
		Expect(inPlaceUpgrade("ami-arm64").Supported).To(BeTrue())
		Expect(snapshotAPI.CallCount()).To(Equal(0))
	})
	It("should mark AMIs with Marketplace product codes as ProductCodes", func() {
		amd64Image.ProductCodes = []ec2types.ProductCode{{ProductCodeId: aws.String("abc123"), ProductCodeType: ec2types.ProductCodeValuesMarketplace}}
		setImages(amd64Image, arm64Image)
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonProductCodes}))
		Expect(inPlaceUpgrade("ami-arm64").Supported).To(BeTrue())
	})
	It("should mark NitroTPM AMIs as NitroTPM", func() {
		arm64Image.TpmSupport = ec2types.TpmSupportValuesV20
		setImages(amd64Image, arm64Image)
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, inPlaceController, nodeClass)
		Expect(inPlaceUpgrade("ami-arm64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonNitroTPM}))
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
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnknown}))
		Expect(inPlaceUpgrade("ami-arm64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnknown}))
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
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnknown}))
	})
	It("should report Unknown for private AMIs when no snapshot API is configured", func() {
		ExpectApplied(ctx, env.Client, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, newController(), nodeClass)
		Expect(inPlaceUpgrade("ami-amd64")).To(Equal(&v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnknown}))
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
		Expect(amd64.InPlaceUpgrade).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
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
		_, err := nodeclass.NewInPlaceUpgradeReconciler(awsEnv.AMIProvider, snapshotAPI).Reconcile(ctx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		Expect(nodeClass.Status.AMIs[0].InPlaceUpgrade).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
		Expect(nodeClass.Status.AMIs[1].InPlaceUpgrade).To(Equal(&v1.InPlaceUpgrade{Supported: true}))
	})
})
