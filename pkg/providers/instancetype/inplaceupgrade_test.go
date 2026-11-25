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

package instancetype_test

import (
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	coretest "sigs.k8s.io/karpenter/pkg/test"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The in-place upgrade result in status.amis is rewritten whenever a gate flips. It must not change the keys of the
// caches that hash status.amis, or every flip would rebuild the instance type list and orphan discovered capacity.
var _ = Describe("InstanceType cache keys and status.amis[].inPlaceUpgrade", func() {
	var nodeClass *v1.EC2NodeClass
	BeforeEach(func() {
		nodeClass = test.EC2NodeClass(v1.EC2NodeClass{
			Spec: v1.EC2NodeClassSpec{AMIDriftPolicy: v1.AMIDriftPolicyReplaceRootVolume},
		})
		// Set after construction: test.EC2NodeClass merges in its own default status.amis.
		nodeClass.Status.AMIs = []v1.AMI{
			{
				ID:           "ami-amd64",
				Name:         "amd64",
				Requirements: []corev1.NodeSelectorRequirement{{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{karpv1.ArchitectureAmd64}}},
			},
			{
				ID:           "ami-arm64",
				Name:         "arm64",
				Requirements: []corev1.NodeSelectorRequirement{{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{karpv1.ArchitectureArm64}}},
			},
		}
		Expect(awsEnv.InstanceTypesProvider.UpdateInstanceTypes(ctx)).To(Succeed())
		Expect(awsEnv.InstanceTypesProvider.UpdateInstanceTypeOfferings(ctx)).To(Succeed())
	})
	setInPlaceUpgrade := func(u *v1.InPlaceUpgrade) {
		for i := range nodeClass.Status.AMIs {
			nodeClass.Status.AMIs[i].InPlaceUpgrade = u.DeepCopy()
		}
	}
	instanceTypeCacheKeys := func() []string {
		_, err := awsEnv.InstanceTypesProvider.List(ctx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		return lo.Keys(awsEnv.InstanceTypeCache.Items())
	}

	It("should keep the instance type cache key when inPlaceUpgrade changes", func() {
		keys := instanceTypeCacheKeys()
		Expect(keys).To(HaveLen(1))
		for _, u := range []*v1.InPlaceUpgrade{
			{Supported: true},
			{Supported: false, Reason: v1.InPlaceUpgradeReasonSnapshotNotReadable},
			{Supported: false, Reason: v1.InPlaceUpgradeReasonUnknown},
			{Supported: false, Reason: v1.InPlaceUpgradeReasonUnknown, Message: "describing the AMI failed", LastTransitionTime: &metav1.Time{Time: time.Now()}},
			{Supported: true, LastTransitionTime: &metav1.Time{Time: time.Now().Add(time.Hour)}},
			nil,
		} {
			setInPlaceUpgrade(u)
			Expect(instanceTypeCacheKeys()).To(ConsistOf(keys))
		}
	})
	It("should keep the instance type cache key when amiDriftPolicy changes", func() {
		keys := instanceTypeCacheKeys()
		nodeClass.Spec.AMIDriftPolicy = v1.AMIDriftPolicyReplaceInstance
		Expect(instanceTypeCacheKeys()).To(ConsistOf(keys))
	})
	It("should still change the instance type cache key when an AMI changes", func() {
		keys := instanceTypeCacheKeys()
		nodeClass.Status.AMIs[0].ID = "ami-amd64-new"
		Expect(instanceTypeCacheKeys()).To(HaveLen(2))
		Expect(lo.Keys(awsEnv.InstanceTypeCache.Items())).To(ContainElements(keys))
	})
	It("should keep the discovered capacity cache key when inPlaceUpgrade or amiDriftPolicy changes", func() {
		its, err := awsEnv.InstanceTypesProvider.List(ctx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		it, ok := lo.Find(its, func(it *corecloudprovider.InstanceType) bool {
			return it.Requirements.Get(corev1.LabelArchStable).Has(karpv1.ArchitectureAmd64)
		})
		Expect(ok).To(BeTrue())
		node := coretest.Node(coretest.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				corev1.LabelInstanceTypeStable: it.Name,
				corev1.LabelArchStable:         karpv1.ArchitectureAmd64,
			}},
			Capacity: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		})
		nodeClaim := coretest.NodeClaim(karpv1.NodeClaim{Status: karpv1.NodeClaimStatus{ImageID: "ami-amd64"}})
		discoveredKeys := func() []string {
			Expect(awsEnv.InstanceTypesProvider.UpdateInstanceTypeCapacityFromNode(ctx, node, nodeClaim, nodeClass)).To(Succeed())
			return lo.Keys(awsEnv.DiscoveredCapacityCache.Items())
		}
		keys := discoveredKeys()
		Expect(keys).To(HaveLen(1))
		setInPlaceUpgrade(&v1.InPlaceUpgrade{Supported: true})
		Expect(discoveredKeys()).To(ConsistOf(keys))
		setInPlaceUpgrade(&v1.InPlaceUpgrade{Supported: false, Reason: v1.InPlaceUpgradeReasonProductCodes})
		Expect(discoveredKeys()).To(ConsistOf(keys))
		nodeClass.Spec.AMIDriftPolicy = v1.AMIDriftPolicyReplaceInstance
		Expect(discoveredKeys()).To(ConsistOf(keys))
	})
})
