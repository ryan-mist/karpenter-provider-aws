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

package cloudprovider_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/arczonalshift"
	arczonalshifttypes "github.com/aws/aws-sdk-go-v2/service/arczonalshift/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/awslabs/operatorpkg/object"
	opstatus "github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
	coretest "sigs.k8s.io/karpenter/pkg/test"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/cloudprovider"
	"github.com/aws/karpenter-provider-aws/pkg/fake"
	"github.com/aws/karpenter-provider-aws/pkg/providers/rootvolume"
	"github.com/aws/karpenter-provider-aws/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

const (
	oldStandardAMI = "ami-old-standard"
	newStandardAMI = "ami-new-standard"
	oldNvidiaAMI   = "ami-old-nvidia"
	newNvidiaAMI   = "ami-new-nvidia"
	newArmAMI      = "ami-new-arm64"
	zoneID         = "tstz1-1a"
)

func eksImageName(variant string, minor int, build string) string {
	return fmt.Sprintf("amazon-eks-node-al2023-x86_64-%s-1.%d-%s", variant, minor, build)
}

var _ = Describe("In-place AMI drift", func() {
	var nodeClass *v1.EC2NodeClass
	var nodePool *karpv1.NodePool
	var nodeClaim *karpv1.NodeClaim
	var node *corev1.Node
	var instance ec2types.Instance
	var images map[string]*ec2types.Image
	var fakeRecorder *record.FakeRecorder
	var rootVolumes *fake.RootVolumeProvider
	var provider *cloudprovider.CloudProvider
	var sg, subnet string

	standardReqs := []corev1.NodeSelectorRequirement{
		{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{karpv1.ArchitectureAmd64}},
		{Key: v1.LabelInstanceGPUCount, Operator: corev1.NodeSelectorOpDoesNotExist},
		{Key: v1.LabelInstanceAcceleratorCount, Operator: corev1.NodeSelectorOpDoesNotExist},
	}
	nvidiaReqs := []corev1.NodeSelectorRequirement{
		{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{karpv1.ArchitectureAmd64}},
		{Key: v1.LabelInstanceGPUCount, Operator: corev1.NodeSelectorOpExists},
	}
	armReqs := []corev1.NodeSelectorRequirement{
		{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{karpv1.ArchitectureArm64}},
	}
	supported := &v1.InPlaceUpgrade{Supported: true}

	image := func(id, name string) *ec2types.Image {
		return &ec2types.Image{
			ImageId:      aws.String(id),
			Name:         aws.String(name),
			Architecture: ec2types.ArchitectureValuesX8664,
			State:        ec2types.ImageStateAvailable,
			CreationDate: aws.String(time.Now().Format(time.RFC3339)),
			EnaSupport:   aws.Bool(true),
		}
	}
	syncImages := func() {
		awsEnv.EC2API.DescribeImagesOutput.Set(&ec2.DescribeImagesOutput{Images: lo.Map(lo.Values(images), func(i *ec2types.Image, _ int) ec2types.Image { return *i })})
		awsEnv.AMIProvider.ResetImages()
	}
	syncInstance := func() {
		awsEnv.EC2API.DescribeInstancesBehavior.Output.Set(&ec2.DescribeInstancesOutput{
			Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{instance}}},
		})
		awsEnv.InstanceCache.Flush()
	}
	target := func(id string) *v1.AMI {
		GinkgoHelper()
		for i := range nodeClass.Status.AMIs {
			if nodeClass.Status.AMIs[i].ID == id {
				return &nodeClass.Status.AMIs[i]
			}
		}
		Fail(fmt.Sprintf("ami %s isn't in status.amis", id))
		return nil
	}
	apply := func() {
		ExpectApplied(ctx, env.Client, nodeClass)
		syncImages()
		syncInstance()
	}
	useInstanceType := func(name string, imageID string) {
		nodeClaim.Labels[corev1.LabelInstanceTypeStable] = name
		instance.InstanceType = ec2types.InstanceType(name)
		instance.ImageId = aws.String(imageID)
		nodeClaim.Status.ImageID = imageID
	}
	driftStatus := func() corecloudprovider.DriftStatus {
		GinkgoHelper()
		s, err := provider.GetDriftStatus(ctx, nodeClaim)
		Expect(err).ToNot(HaveOccurred())
		return s
	}
	expectFallback := func(gate string) {
		GinkgoHelper()
		s := driftStatus()
		Expect(s).To(Equal(corecloudprovider.DriftStatus{Action: corecloudprovider.ReplaceNode, Reason: cloudprovider.AMIDrift}))
		Expect(fakeRecorder.Events).To(Receive(And(ContainSubstring("InPlaceUpgradeFallback"), ContainSubstring(gate))))
	}
	expectRebootNode := func() {
		GinkgoHelper()
		Expect(driftStatus()).To(Equal(corecloudprovider.DriftStatus{Action: corecloudprovider.RebootNode, Reason: cloudprovider.AMIDrift}))
	}
	setRebooting := func(reason string, status metav1.ConditionStatus) {
		if status == metav1.ConditionTrue {
			nodeClaim.StatusConditions().SetTrueWithReason(karpv1.ConditionTypeRebooting, reason, reason)
		} else {
			nodeClaim.StatusConditions().SetFalse(karpv1.ConditionTypeRebooting, reason, reason)
		}
	}
	task := func(imageID string, state ec2types.ReplaceRootVolumeTaskState, token string) *rootvolume.Task {
		return &rootvolume.Task{
			ID:           "replacevol-" + coretest.RandomName(),
			InstanceID:   aws.ToString(instance.InstanceId),
			ImageID:      imageID,
			State:        state,
			StartTime:    time.Now().Add(-time.Minute),
			CompleteTime: lo.Ternary(state == ec2types.ReplaceRootVolumeTaskStatePending, time.Time{}, time.Now()),
			Token:        token,
		}
	}

	BeforeEach(func() {
		sg, subnet = fake.SecurityGroupID(), fake.SubnetID()
		fakeRecorder = record.NewFakeRecorder(100)
		rootVolumes = fake.NewRootVolumeProvider()
		provider = cloudprovider.New(awsEnv.InstanceTypesProvider, awsEnv.InstanceProvider, events.NewRecorder(fakeRecorder), env.Client,
			awsEnv.AMIProvider, awsEnv.SecurityGroupProvider, awsEnv.CapacityReservationProvider, awsEnv.PlacementGroupProvider,
			awsEnv.InstanceTypeStore, testCABundle, rootVolumes)

		nodeClass = test.EC2NodeClass(v1.EC2NodeClass{Spec: v1.EC2NodeClassSpec{AMIDriftPolicy: v1.AMIDriftPolicyReplaceRootVolume}})
		nodeClass.Status = v1.EC2NodeClassStatus{
			InstanceProfile: "test-profile",
			Subnets:         []v1.Subnet{{ID: subnet, Zone: "test-zone-1a", ZoneID: zoneID}},
			SecurityGroups:  []v1.SecurityGroup{{ID: sg}},
			AMIs: []v1.AMI{
				{ID: newStandardAMI, Name: eksImageName("standard", 33, "v2"), Requirements: standardReqs, InPlaceUpgrade: supported.DeepCopy()},
				{ID: newNvidiaAMI, Name: eksImageName("nvidia", 33, "v2"), Requirements: nvidiaReqs, InPlaceUpgrade: supported.DeepCopy()},
				{ID: newArmAMI, Name: "amazon-eks-node-al2023-arm64-standard-1.33-v2", Requirements: armReqs, InPlaceUpgrade: supported.DeepCopy()},
			},
		}
		nodeClass.StatusConditions().SetTrue(opstatus.ConditionReady)
		nodeClass.Annotations = lo.Assign(nodeClass.Annotations, map[string]string{
			v1.AnnotationEC2NodeClassHash:        nodeClass.Hash(testCABundle),
			v1.AnnotationEC2NodeClassHashVersion: v1.EC2NodeClassHashVersion,
		})
		nodePool = coretest.NodePool(karpv1.NodePool{Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: object.GVK(nodeClass).Group, Kind: object.GVK(nodeClass).Kind, Name: nodeClass.Name},
		}}}})
		images = map[string]*ec2types.Image{
			oldStandardAMI: image(oldStandardAMI, eksImageName("standard", 33, "v1")),
			newStandardAMI: image(newStandardAMI, eksImageName("standard", 33, "v2")),
			oldNvidiaAMI:   image(oldNvidiaAMI, eksImageName("nvidia", 33, "v1")),
			newNvidiaAMI:   image(newNvidiaAMI, eksImageName("nvidia", 33, "v2")),
		}
		instance = ec2types.Instance{
			InstanceId:              aws.String(fake.InstanceID()),
			ImageId:                 aws.String(oldStandardAMI),
			InstanceType:            "m5.large",
			SubnetId:                aws.String(subnet),
			SecurityGroups:          []ec2types.GroupIdentifier{{GroupId: aws.String(sg)}},
			State:                   &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
			Placement:               &ec2types.Placement{AvailabilityZone: aws.String("test-zone-1a"), AvailabilityZoneId: aws.String(zoneID)},
			EnaSupport:              aws.Bool(true),
			CurrentInstanceBootMode: ec2types.InstanceBootModeValuesLegacyBios,
		}
		node = coretest.Node(coretest.NodeOptions{})
		node.Status.NodeInfo.KubeletVersion = "v1.33.5-eks-1234567"
		nodeClaim = coretest.NodeClaim(karpv1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{karpv1.NodePoolLabelKey: nodePool.Name, corev1.LabelInstanceTypeStable: "m5.large"},
				Annotations: map[string]string{
					v1.AnnotationEC2NodeClassHash:        nodeClass.Hash(testCABundle),
					v1.AnnotationEC2NodeClassHashVersion: v1.EC2NodeClassHashVersion,
				},
			},
			Status: karpv1.NodeClaimStatus{
				ProviderID: fake.ProviderID(aws.ToString(instance.InstanceId)),
				ImageID:    oldStandardAMI,
				NodeName:   node.Name,
			},
		})
		Expect(awsEnv.InstanceTypesProvider.UpdateInstanceTypes(ctx)).To(Succeed())
		Expect(awsEnv.InstanceTypesProvider.UpdateInstanceTypeOfferings(ctx)).To(Succeed())
		ExpectApplied(ctx, env.Client, nodePool, node)
		apply()
	})

	Context("GetDriftStatus", func() {
		It("should replace AMI drift when amiDriftPolicy is ReplaceInstance, without reading root volume tasks", func() {
			nodeClass.Spec.AMIDriftPolicy = v1.AMIDriftPolicyReplaceInstance
			apply()
			rootVolumes.LatestError = fmt.Errorf("must not be called")
			setRebooting(karpv1.RebootReasonSucceeded, metav1.ConditionFalse)
			Expect(driftStatus()).To(Equal(corecloudprovider.DriftStatus{Action: corecloudprovider.ReplaceNode, Reason: cloudprovider.AMIDrift}))
			Expect(fakeRecorder.Events).ToNot(Receive())
		})
		It("should keep today's behavior when no root volume provider is configured", func() {
			provider = cloudprovider.New(awsEnv.InstanceTypesProvider, awsEnv.InstanceProvider, events.NewRecorder(fakeRecorder), env.Client,
				awsEnv.AMIProvider, awsEnv.SecurityGroupProvider, awsEnv.CapacityReservationProvider, awsEnv.PlacementGroupProvider,
				awsEnv.InstanceTypeStore, testCABundle, nil)
			Expect(driftStatus()).To(Equal(corecloudprovider.DriftStatus{Action: corecloudprovider.ReplaceNode, Reason: cloudprovider.AMIDrift}))
		})
		It("should return the same first reason as IsDrifted for other drift on a ReplaceInstance EC2NodeClass", func() {
			nodeClass.Spec.AMIDriftPolicy = v1.AMIDriftPolicyReplaceInstance
			instance.SubnetId = aws.String(fake.SubnetID())
			apply()
			reason, err := provider.IsDrifted(ctx, nodeClaim)
			Expect(err).ToNot(HaveOccurred())
			// IsDrifted reports AMI drift first.
			Expect(reason).To(Equal(cloudprovider.AMIDrift))
			Expect(driftStatus()).To(Equal(corecloudprovider.DriftStatus{Action: corecloudprovider.ReplaceNode, Reason: cloudprovider.AMIDrift}))
		})
		It("should reboot to resolve AMI-only drift when every gate passes", func() {
			expectRebootNode()
			Expect(fakeRecorder.Events).ToNot(Receive())
		})
		It("should report not drifted when the node runs the target AMI", func() {
			useInstanceType("m5.large", newStandardAMI)
			apply()
			Expect(driftStatus()).To(Equal(corecloudprovider.NotDrifted))
		})
		It("should replace when AMI drift comes with another drift reason, reporting the other reason", func() {
			instance.SubnetId = aws.String(fake.SubnetID())
			apply()
			s := driftStatus()
			Expect(s).To(Equal(corecloudprovider.DriftStatus{Action: corecloudprovider.ReplaceNode, Reason: cloudprovider.SubnetDrift}))
			Expect(fakeRecorder.Events).To(Receive(ContainSubstring(cloudprovider.InPlaceGateNotAMIOnly)))
		})
		It("should replace without an in-place event when only non-AMI drift is present", func() {
			useInstanceType("m5.large", newStandardAMI)
			instance.SecurityGroups = []ec2types.GroupIdentifier{{GroupId: aws.String(fake.SecurityGroupID())}}
			apply()
			Expect(driftStatus()).To(Equal(corecloudprovider.DriftStatus{Action: corecloudprovider.ReplaceNode, Reason: cloudprovider.SecurityGroupDrift}))
			Expect(fakeRecorder.Events).ToNot(Receive())
		})
		It("should replace on static drift", func() {
			nodeClaim.Annotations[v1.AnnotationEC2NodeClassHash] = "different"
			Expect(driftStatus()).To(Equal(corecloudprovider.DriftStatus{Action: corecloudprovider.ReplaceNode, Reason: cloudprovider.NodeClassDrift}))
		})
		It("should replace when no AMI is compatible with the node's instance type", func() {
			nodeClass.Status.AMIs = lo.Filter(nodeClass.Status.AMIs, func(a v1.AMI, _ int) bool { return a.ID == newArmAMI })
			apply()
			expectFallback(cloudprovider.InPlaceGateNoTargetAMI)
		})
		It("should dedupe the fallback event", func() {
			target(newStandardAMI).InPlaceUpgrade = &v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonSnapshotNotReadable}
			apply()
			expectFallback(v1.InPlaceUpgradeReasonSnapshotNotReadable)
			driftStatus()
			Expect(fakeRecorder.Events).ToNot(Receive())
		})

		Context("Target AMI", func() {
			It("should target the variant matching a GPU instance type", func() {
				useInstanceType("g4dn.8xlarge", oldNvidiaAMI)
				apply()
				expectRebootNode()
				Expect(provider.Reboot(ctx, drifted(nodeClaim), "op")).To(Succeed())
				Expect(rootVolumes.CreateCalls).To(HaveLen(1))
				Expect(rootVolumes.CreateCalls[0].ImageID).To(Equal(newNvidiaAMI))
			})
			It("should target the standard variant for a standard instance type", func() {
				Expect(provider.Reboot(ctx, drifted(nodeClaim), "op")).To(Succeed())
				Expect(rootVolumes.CreateCalls[0].ImageID).To(Equal(newStandardAMI))
			})
			It("should replace a GPU node whose current image is the standard variant", func() {
				useInstanceType("g4dn.8xlarge", oldStandardAMI)
				apply()
				expectFallback(cloudprovider.InPlaceGateVariant)
			})
			It("should prefer the current image's status.amis entry over its name", func() {
				// The node's image is still listed, as the nvidia entry for its instance type, though its name says
				// standard. The entry decides, so the nvidia target is the same variant.
				nodeClass.Status.AMIs = append(nodeClass.Status.AMIs, v1.AMI{ID: oldStandardAMI, Name: "custom", Requirements: nvidiaReqs, InPlaceUpgrade: supported.DeepCopy()})
				useInstanceType("g4dn.8xlarge", oldStandardAMI)
				apply()
				expectRebootNode()
			})
			It("should derive the current variant from the instance type when the current image is gone", func() {
				delete(images, oldStandardAMI)
				apply()
				expectRebootNode()
			})
			It("should derive a GPU variant from the instance type when the current image is gone", func() {
				delete(images, oldNvidiaAMI)
				nodeClaim.Labels[v1.LabelInstanceGPUCount] = "1"
				useInstanceType("g4dn.8xlarge", oldNvidiaAMI)
				apply()
				expectRebootNode()
			})
			It("should replace when the current image is gone and the NodeClaim's accelerators don't match the target", func() {
				delete(images, oldNvidiaAMI)
				useInstanceType("g4dn.8xlarge", oldNvidiaAMI)
				apply()
				expectFallback(cloudprovider.InPlaceGateVariant)
			})
		})

		Context("AMI gates", func() {
			DescribeTable("should replace and name the status.amis reason",
				func(reason string) {
					target(newStandardAMI).InPlaceUpgrade = &v1.InPlaceUpgrade{Reason: reason}
					apply()
					expectFallback(reason)
				},
				Entry("product codes", v1.InPlaceUpgradeReasonProductCodes),
				Entry("snapshot", v1.InPlaceUpgradeReasonSnapshotNotReadable),
				Entry("NitroTPM", v1.InPlaceUpgradeReasonNitroTPM),
				Entry("family", v1.InPlaceUpgradeReasonUnsupportedAMIFamily),
			)
			It("should wait while the target AMI hasn't been checked", func() {
				target(newStandardAMI).InPlaceUpgrade = nil
				apply()
				_, err := provider.GetDriftStatus(ctx, nodeClaim)
				Expect(err).To(HaveOccurred())
			})
			It("should wait while the target AMI's checks couldn't complete", func() {
				target(newStandardAMI).InPlaceUpgrade = &v1.InPlaceUpgrade{Reason: v1.InPlaceUpgradeReasonUnknown}
				apply()
				_, err := provider.GetDriftStatus(ctx, nodeClaim)
				Expect(err).To(HaveOccurred())
				Expect(fakeRecorder.Events).ToNot(Receive())
			})
		})

		Context("Node gates", func() {
			It("should replace bare metal instances", func() {
				useInstanceType("m5.metal", oldStandardAMI)
				apply()
				expectFallback(cloudprovider.InPlaceGateBareMetal)
			})
			It("should replace instances that aren't running", func() {
				instance.State = &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}
				apply()
				expectFallback(cloudprovider.InPlaceGateInstanceNotRunning)
			})
			It("should replace instances with product codes", func() {
				instance.ProductCodes = []ec2types.ProductCode{{ProductCodeId: aws.String("abc")}}
				apply()
				expectFallback(cloudprovider.InPlaceGateInstanceProductCodes)
			})
			It("should replace instances with NitroTPM enabled", func() {
				instance.TpmSupport = aws.String("v2.0")
				apply()
				expectFallback(cloudprovider.InPlaceGateInstanceNitroTPM)
			})
			It("should replace when the target AMI requires a different boot mode", func() {
				images[newStandardAMI].BootMode = ec2types.BootModeValuesUefi
				apply()
				expectFallback(cloudprovider.InPlaceGateBootMode)
			})
			DescribeTable("should allow boot modes EC2 accepts",
				func(amiMode ec2types.BootModeValues, instanceMode ec2types.InstanceBootModeValues) {
					images[newStandardAMI].BootMode = amiMode
					instance.CurrentInstanceBootMode = instanceMode
					apply()
					expectRebootNode()
				},
				Entry("unset", ec2types.BootModeValues(""), ec2types.InstanceBootModeValuesLegacyBios),
				Entry("uefi-preferred", ec2types.BootModeValuesUefiPreferred, ec2types.InstanceBootModeValuesUefi),
				Entry("matching legacy-bios", ec2types.BootModeValuesLegacyBios, ec2types.InstanceBootModeValuesLegacyBios),
				Entry("matching uefi", ec2types.BootModeValuesUefi, ec2types.InstanceBootModeValuesUefi),
			)
			It("should replace an ENA instance when the target AMI lacks ENA", func() {
				images[newStandardAMI].EnaSupport = aws.Bool(false)
				apply()
				expectFallback(cloudprovider.InPlaceGateENA)
			})
			It("should allow a non-ENA instance to move to an ENA AMI", func() {
				instance.EnaSupport = aws.Bool(false)
				apply()
				expectRebootNode()
			})
			It("should replace an sriov-net instance when the target AMI lacks it", func() {
				instance.SriovNetSupport = aws.String("simple")
				apply()
				expectFallback(cloudprovider.InPlaceGateSriovNet)
			})
			It("should replace when the target AMI is for a different Kubernetes minor", func() {
				target(newStandardAMI).Name = eksImageName("standard", 34, "v2")
				apply()
				expectFallback(cloudprovider.InPlaceGateKubernetesVersion)
			})
			It("should take the current minor from the kubelet over the image name", func() {
				node.Status.NodeInfo.KubeletVersion = "v1.32.9"
				ExpectApplied(ctx, env.Client, node)
				expectFallback(cloudprovider.InPlaceGateKubernetesVersion)
			})
			It("should fall back to the current image name when the node is gone", func() {
				nodeClaim.Status.NodeName = ""
				expectRebootNode()
			})
			It("should replace when the current Kubernetes minor is unknown", func() {
				nodeClaim.Status.NodeName = ""
				delete(images, oldStandardAMI)
				apply()
				expectFallback(cloudprovider.InPlaceGateKubernetesVersionUnknown)
			})
			It("should replace when the target AMI's Kubernetes minor is unknown", func() {
				target(newStandardAMI).Name = "my-custom-ami"
				images[newStandardAMI].Name = aws.String("my-custom-ami")
				apply()
				expectFallback(cloudprovider.InPlaceGateKubernetesVersionUnknown)
			})
			It("should return an error instead of draining in a zonally shifted availability zone", func() {
				shiftZone(zoneID)
				_, err := provider.GetDriftStatus(ctx, nodeClaim)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("zonally shifted"))
			})
			It("should still replace for non-AMI drift in a zonally shifted availability zone", func() {
				shiftZone(zoneID)
				instance.SubnetId = aws.String(fake.SubnetID())
				apply()
				Expect(driftStatus().Action).To(Equal(corecloudprovider.ReplaceNode))
			})
		})

		Context("Rebooting", func() {
			It("should not read root volume tasks for NodeClaims that never rebooted", func() {
				rootVolumes.LatestError = fmt.Errorf("must not be called")
				expectRebootNode()
			})
			It("should evaluate normally when no task exists", func() {
				setRebooting(karpv1.RebootReasonSucceeded, metav1.ConditionFalse)
				expectRebootNode()
			})
			It("should return an error while the task is running", func() {
				setRebooting(karpv1.RebootReasonIssued, metav1.ConditionTrue)
				rootVolumes.AddTask(task(newStandardAMI, ec2types.ReplaceRootVolumeTaskStateInProgress, "token"))
				_, err := provider.GetDriftStatus(ctx, nodeClaim)
				Expect(err).To(HaveOccurred())
			})
			It("should return an error when the task can't be read", func() {
				setRebooting(karpv1.RebootReasonSucceeded, metav1.ConditionFalse)
				rootVolumes.LatestError = fmt.Errorf("throttled")
				_, err := provider.GetDriftStatus(ctx, nodeClaim)
				Expect(err).To(HaveOccurred())
			})
			It("should evaluate against the succeeded task's image before status.imageID is updated", func() {
				setRebooting(karpv1.RebootReasonSucceeded, metav1.ConditionFalse)
				rootVolumes.AddTask(task(newStandardAMI, ec2types.ReplaceRootVolumeTaskStateSucceeded, "token"))
				Expect(driftStatus()).To(Equal(corecloudprovider.NotDrifted))
			})
			It("should reboot again when the succeeded task's image has since drifted", func() {
				setRebooting(karpv1.RebootReasonSucceeded, metav1.ConditionFalse)
				images["ami-older"] = image("ami-older", eksImageName("standard", 33, "v0"))
				apply()
				rootVolumes.AddTask(task("ami-older", ec2types.ReplaceRootVolumeTaskStateSucceeded, "token"))
				expectRebootNode()
			})
			DescribeTable("should replace when our task toward the current target failed",
				func(state ec2types.ReplaceRootVolumeTaskState) {
					setRebooting(karpv1.RebootReasonSucceeded, metav1.ConditionFalse)
					rootVolumes.AddTask(task(newStandardAMI, state, "token"))
					expectFallback(cloudprovider.InPlaceGatePreviousAttemptFailed)
				},
				Entry("failed", ec2types.ReplaceRootVolumeTaskStateFailed),
				Entry("failed-detached", ec2types.ReplaceRootVolumeTaskState("failed-detached")),
			)
			It("should retry in place when the failed task was toward an older target", func() {
				setRebooting(karpv1.RebootReasonSucceeded, metav1.ConditionFalse)
				rootVolumes.AddTask(task("ami-older", ec2types.ReplaceRootVolumeTaskStateFailed, "token"))
				expectRebootNode()
			})
			It("should ignore tasks Karpenter didn't create", func() {
				setRebooting(karpv1.RebootReasonSucceeded, metav1.ConditionFalse)
				rootVolumes.AddTask(task(newStandardAMI, ec2types.ReplaceRootVolumeTaskStateInProgress, ""))
				expectRebootNode()
			})
		})
	})

	Context("Reboot", func() {
		const operationID = "0b1a6f2e-3c4d-4e5f-8a9b-0c1d2e3f4a5b-1763683200000000000-8f14e45f-ceea-467f-a0e6-1c3b2b9a7d20"
		token := func() string {
			sum := sha256.Sum256([]byte(operationID))
			return hex.EncodeToString(sum[:])
		}()
		BeforeEach(func() {
			nodeClaim = drifted(nodeClaim)
			setRebooting(karpv1.RebootReasonRequested, metav1.ConditionTrue)
		})
		It("should replace the root volume from the target AMI with a 64 character token", func() {
			Expect(provider.Reboot(ctx, nodeClaim, operationID)).To(Succeed())
			Expect(rootVolumes.CreateCalls).To(Equal([]fake.CreateRootVolumeTaskInput{{InstanceID: aws.ToString(instance.InstanceId), ImageID: newStandardAMI, Token: token}}))
			Expect(token).To(HaveLen(64))
			Expect(awsEnv.EC2API.RebootInstancesBehavior.CalledWithInput.Len()).To(Equal(0))
		})
		It("should use the same token on every retry", func() {
			Expect(provider.Reboot(ctx, nodeClaim, operationID)).To(Succeed())
			Expect(provider.Reboot(ctx, nodeClaim, operationID)).To(Succeed())
			Expect(rootVolumes.CreateCalls).To(HaveLen(2))
			Expect(rootVolumes.CreateCalls[1].Token).To(Equal(rootVolumes.CreateCalls[0].Token))
			latest, err := rootVolumes.Latest(ctx, aws.ToString(instance.InstanceId))
			Expect(err).ToNot(HaveOccurred())
			Expect(latest.Token).To(Equal(token))
		})
		It("should adopt this operation's running task even if the target changed since", func() {
			rootVolumes.AddTask(task(newStandardAMI, ec2types.ReplaceRootVolumeTaskStateInProgress, token))
			target(newStandardAMI).ID = "ami-newer"
			apply()
			Expect(provider.Reboot(ctx, nodeClaim, operationID)).To(Succeed())
			Expect(rootVolumes.CreateCalls).To(HaveLen(1))
			Expect(rootVolumes.CreateCalls[0].ImageID).To(Equal(newStandardAMI))
		})
		It("should plainly reboot when the reboot isn't for drift", func() {
			nodeClaim.StatusConditions().SetTrueWithReason(karpv1.ConditionTypeDisruptionReason, string(karpv1.DisruptionReasonUnhealthy), string(karpv1.DisruptionReasonUnhealthy))
			Expect(provider.Reboot(ctx, nodeClaim, operationID)).To(Succeed())
			Expect(rootVolumes.CreateCalls).To(BeEmpty())
			Expect(awsEnv.EC2API.RebootInstancesBehavior.CalledWithInput.Len()).To(Equal(1))
		})
		It("should plainly reboot without a disruption reason", func() {
			_ = nodeClaim.StatusConditions().Clear(karpv1.ConditionTypeDisruptionReason)
			Expect(provider.Reboot(ctx, nodeClaim, operationID)).To(Succeed())
			Expect(rootVolumes.CreateCalls).To(BeEmpty())
			Expect(awsEnv.EC2API.RebootInstancesBehavior.CalledWithInput.Len()).To(Equal(1))
		})
		It("should plainly reboot when the EC2NodeClass uses ReplaceInstance", func() {
			nodeClass.Spec.AMIDriftPolicy = v1.AMIDriftPolicyReplaceInstance
			apply()
			Expect(provider.Reboot(ctx, nodeClaim, operationID)).To(Succeed())
			Expect(rootVolumes.CreateCalls).To(BeEmpty())
			Expect(awsEnv.EC2API.RebootInstancesBehavior.CalledWithInput.Len()).To(Equal(1))
		})
		It("should plainly reboot when the node is no longer drifted", func() {
			useInstanceType("m5.large", newStandardAMI)
			apply()
			Expect(provider.Reboot(ctx, nodeClaim, operationID)).To(Succeed())
			Expect(rootVolumes.CreateCalls).To(BeEmpty())
			Expect(awsEnv.EC2API.RebootInstancesBehavior.CalledWithInput.Len()).To(Equal(1))
		})
		It("should fail the reboot when a gate fails at issue time", func() {
			target(newStandardAMI).Name = eksImageName("standard", 34, "v2")
			apply()
			err := provider.Reboot(ctx, nodeClaim, operationID)
			Expect(corecloudprovider.IsNodeRebootFailedError(err)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring(cloudprovider.InPlaceGateKubernetesVersion))
			Expect(rootVolumes.CreateCalls).To(BeEmpty())
			Expect(awsEnv.EC2API.RebootInstancesBehavior.CalledWithInput.Len()).To(Equal(0))
		})
		It("should fail the reboot when the node picked up another drift reason", func() {
			instance.SubnetId = aws.String(fake.SubnetID())
			apply()
			Expect(corecloudprovider.IsNodeRebootFailedError(provider.Reboot(ctx, nodeClaim, operationID))).To(BeTrue())
			Expect(rootVolumes.CreateCalls).To(BeEmpty())
		})
		It("should retry while the target AMI's checks are pending", func() {
			target(newStandardAMI).InPlaceUpgrade = nil
			apply()
			err := provider.Reboot(ctx, nodeClaim, operationID)
			Expect(err).To(HaveOccurred())
			Expect(corecloudprovider.IsNodeRebootFailedError(err)).To(BeFalse())
			Expect(rootVolumes.CreateCalls).To(BeEmpty())
		})
		It("should retry without calling EC2 in a zonally shifted availability zone", func() {
			shiftZone(zoneID)
			err := provider.Reboot(ctx, nodeClaim, operationID)
			Expect(err).To(HaveOccurred())
			Expect(corecloudprovider.IsNodeRebootFailedError(err)).To(BeFalse())
			Expect(err.Error()).To(ContainSubstring("zonally shifted"))
			Expect(rootVolumes.CreateCalls).To(BeEmpty())
			Expect(awsEnv.EC2API.RebootInstancesBehavior.CalledWithInput.Len()).To(Equal(0))
		})
		DescribeTable("should fail the reboot on deterministic EC2 errors",
			func(code string) {
				rootVolumes.CreateError = &smithy.GenericAPIError{Code: code, Message: "nope"}
				err := provider.Reboot(ctx, nodeClaim, operationID)
				Expect(corecloudprovider.IsNodeRebootFailedError(err)).To(BeTrue())
			},
			Entry("UnauthorizedOperation", "UnauthorizedOperation"),
			Entry("InvalidParameterValue", "InvalidParameterValue"),
			Entry("InvalidParameterCombination", "InvalidParameterCombination"),
			Entry("InvalidSnapshot.NotFound", "InvalidSnapshot.NotFound"),
			Entry("InvalidAMIID.NotFound", "InvalidAMIID.NotFound"),
			Entry("InvalidAMIID.Malformed", "InvalidAMIID.Malformed"),
			Entry("OperationNotPermitted", "OperationNotPermitted"),
			Entry("IdempotentParameterMismatch", "IdempotentParameterMismatch"),
		)
		DescribeTable("should retry transient EC2 errors",
			func(err error) {
				rootVolumes.CreateError = err
				out := provider.Reboot(ctx, nodeClaim, operationID)
				Expect(out).To(HaveOccurred())
				Expect(corecloudprovider.IsNodeRebootFailedError(out)).To(BeFalse())
				Expect(corecloudprovider.IsNodeClaimNotFoundError(out)).To(BeFalse())
			},
			Entry("throttling", &smithy.GenericAPIError{Code: "RequestLimitExceeded"}),
			Entry("server error", &smithy.GenericAPIError{Code: "InternalError", Fault: smithy.FaultServer}),
			Entry("unavailable", &smithy.GenericAPIError{Code: "Unavailable", Fault: smithy.FaultServer}),
			Entry("a task already running", &smithy.GenericAPIError{Code: "IncorrectInstanceState"}),
			Entry("state change in progress", &smithy.GenericAPIError{Code: "IncorrectState"}),
			Entry("a non-API error", fmt.Errorf("connection reset")),
		)
		It("should report a terminated instance as not found", func() {
			rootVolumes.CreateError = &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound"}
			Expect(corecloudprovider.IsNodeClaimNotFoundError(provider.Reboot(ctx, nodeClaim, operationID))).To(BeTrue())
		})
	})
})

// drifted marks the NodeClaim as disrupted for drift, as core does when it commits a drift reboot.
func drifted(nodeClaim *karpv1.NodeClaim) *karpv1.NodeClaim {
	nodeClaim.StatusConditions().SetTrueWithReason(karpv1.ConditionTypeDisruptionReason, string(karpv1.DisruptionReasonDrifted), string(karpv1.DisruptionReasonDrifted))
	return nodeClaim
}

func shiftZone(id string) {
	GinkgoHelper()
	awsEnv.ARCZonalShiftAPI.GetManagedResourceBehavior.Output.Set(&arczonalshift.GetManagedResourceOutput{
		ZonalShifts: []arczonalshifttypes.ZonalShiftInResource{{
			AwayFrom:      aws.String(id),
			ExpiryTime:    aws.Time(time.Now().Add(time.Hour)),
			AppliedStatus: arczonalshifttypes.AppliedStatusApplied,
		}},
	})
	Expect(awsEnv.ZonalShiftProvider.UpdateZonalShifts(ctx)).To(Succeed())
}
