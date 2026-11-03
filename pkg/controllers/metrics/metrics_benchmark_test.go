//go:build test_performance

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

package metrics_test

import (
	"fmt"
	"testing"

	"github.com/awslabs/operatorpkg/object"
	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	coretest "sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	"github.com/aws/karpenter-provider-aws/pkg/apis"
	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/cloudprovider"
	"github.com/aws/karpenter-provider-aws/pkg/controllers/metrics"
	"github.com/aws/karpenter-provider-aws/pkg/fake"
	"github.com/aws/karpenter-provider-aws/pkg/operator/options"
	"github.com/aws/karpenter-provider-aws/pkg/test"
)

// BenchmarkReconcile compares resolving instance types for every NodePool (the previous behavior, reproduced by
// perNodePoolReference) with the controller's per-NodeClass resolution, for clusters with many NodePools that
// share a few NodeClasses.
//
//	go test -tags test_performance -run '^$' -bench BenchmarkReconcile -benchmem ./pkg/controllers/metrics/
func BenchmarkReconcile(b *testing.B) {
	ctx := TestContextWithLogger(b)
	ctx = coreoptions.ToContext(ctx, coretest.Options(coretest.OptionsFields{FeatureGates: coretest.FeatureGates{ReservedCapacity: lo.ToPtr(true)}}))
	ctx = options.ToContext(ctx, test.Options())
	env := coretest.NewEnvironment(coretest.WithCRDs(apis.CRDs...), coretest.WithCRDs(v1alpha1.CRDs...))
	defer func() { lo.Must0(env.Stop()) }()
	awsEnv := test.NewEnvironment(ctx, env)
	cp := cloudprovider.New(awsEnv.InstanceTypesProvider, awsEnv.InstanceProvider, events.NewRecorder(&record.FakeRecorder{}),
		env.Client, awsEnv.AMIProvider, awsEnv.SecurityGroupProvider, awsEnv.CapacityReservationProvider, awsEnv.PlacementGroupProvider, awsEnv.InstanceTypeStore, lo.ToPtr(""))
	spotPriceHistory, pricingData := fake.GenerateDefaultPriceOutput()
	awsEnv.PricingAPI.GetProductsBehavior.Output.Set(pricingData)
	awsEnv.EC2API.DescribeSpotPriceHistoryBehavior.Output.Set(spotPriceHistory)
	lo.Must0(awsEnv.PricingProvider.UpdateOnDemandPricing(ctx))
	lo.Must0(awsEnv.PricingProvider.UpdateSpotPricing(ctx))

	const nodePoolCount, nodeClassCount = 500, 5
	nodeClasses := make([]*v1.EC2NodeClass, nodeClassCount)
	for i := range nodeClasses {
		nodeClasses[i] = test.EC2NodeClass(v1.EC2NodeClass{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("nodeclass-%d", i)},
			Status: v1.EC2NodeClassStatus{Subnets: []v1.Subnet{
				{ID: "subnet-test1", Zone: "test-zone-1a"},
				{ID: "subnet-test2", Zone: "test-zone-1b"},
				{ID: "subnet-test3", Zone: "test-zone-1c"},
			}},
		})
		nodeClassStatus := nodeClasses[i].Status.DeepCopy()
		lo.Must0(env.Client.Create(ctx, nodeClasses[i]))
		nodeClasses[i].Status = *nodeClassStatus
		nodeClasses[i].StatusConditions().SetTrue(status.ConditionReady)
		lo.Must0(env.Client.Status().Update(ctx, nodeClasses[i]))
		lo.Must(awsEnv.SubnetProvider.List(ctx, nodeClasses[i]))
	}
	for i := range nodePoolCount {
		nc := nodeClasses[i%nodeClassCount]
		np := coretest.NodePool(karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("nodepool-%03d", i)}})
		np.Spec.Template.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: object.GVK(nc).Group, Kind: object.GVK(nc).Kind, Name: nc.Name}
		lo.Must0(env.Client.Create(ctx, np))
	}
	lo.Must0(awsEnv.InstanceTypesProvider.UpdateInstanceTypes(ctx))
	lo.Must0(awsEnv.InstanceTypesProvider.UpdateInstanceTypeOfferings(ctx))

	b.Run(fmt.Sprintf("PerNodePool/nodepools=%d/nodeclasses=%d", nodePoolCount, nodeClassCount), func(b *testing.B) {
		counting := &countingCloudProvider{CloudProvider: cp}
		b.ReportAllocs()
		for b.Loop() {
			lo.Must2(perNodePoolReference(ctx, env.Client, counting))
		}
		b.ReportMetric(float64(counting.calls.Load())/float64(b.N), "GetInstanceTypes/op")
	})
	b.Run(fmt.Sprintf("PerNodeClass/nodepools=%d/nodeclasses=%d", nodePoolCount, nodeClassCount), func(b *testing.B) {
		counting := &countingCloudProvider{CloudProvider: cp}
		controller := metrics.NewController(env.Client, counting, awsEnv.InstanceTypeStore)
		b.ReportAllocs()
		for b.Loop() {
			lo.Must(controller.Reconcile(ctx))
		}
		b.ReportMetric(float64(counting.calls.Load())/float64(b.N), "GetInstanceTypes/op")
	})
}
