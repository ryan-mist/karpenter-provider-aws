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

package offering_test

import (
	"context"
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/aws/karpenter-provider-aws/pkg/providers/instancetype/offering"
	"github.com/aws/karpenter-provider-aws/pkg/providers/placementgroup"
)

func TestOffering(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Offering")
}

var _ = Describe("PlacementGroupResolver", func() {
	It("should carry Available and Incompatible onto each partition's offering", func() {
		offerings := cloudprovider.Offerings{
			{
				Requirements: scheduling.NewLabelRequirements(map[string]string{karpv1.CapacityTypeLabelKey: karpv1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}),
				Available:    false,
				Incompatible: true,
			},
			{
				Requirements: scheduling.NewLabelRequirements(map[string]string{karpv1.CapacityTypeLabelKey: karpv1.CapacityTypeSpot, corev1.LabelTopologyZone: "test-zone-1a"}),
				Available:    false,
			},
		}
		pg := &placementgroup.PlacementGroup{ID: "pg-1", Strategy: placementgroup.StrategyPartition, PartitionCount: 2}
		expanded := (&offering.PlacementGroupResolver{}).ResolveOfferings(context.Background(), nil, offerings, ec2types.InstanceTypeInfo{}, nil, nil, nil, pg, nil)
		Expect(expanded).To(HaveLen(4))
		for _, o := range expanded {
			Expect(o.Available).To(BeFalse())
			Expect(o.Incompatible).To(Equal(o.CapacityType() == karpv1.CapacityTypeOnDemand))
		}
	})
})
