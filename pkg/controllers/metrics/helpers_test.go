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
	"context"
	"sync/atomic"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
)

const (
	availableMetricName = "karpenter_cloudprovider_instance_type_offering_available"
	priceMetricName     = "karpenter_cloudprovider_instance_type_offering_price_estimate"
)

// countingCloudProvider counts GetInstanceTypes calls made through it.
type countingCloudProvider struct {
	corecloudprovider.CloudProvider
	calls atomic.Int64
}

func (c *countingCloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*corecloudprovider.InstanceType, error) {
	c.calls.Add(1)
	return c.CloudProvider.GetInstanceTypes(ctx, nodePool)
}

type series struct {
	instanceType string
	capacityType string
	zone         string
}

// perNodePoolReference reproduces the controller's previous algorithm: every NodePool is resolved through the given
// (overlay-decorated) CloudProvider and aggregated in NodePool list order. It returns the gauge values that
// algorithm would have emitted.
func perNodePoolReference(ctx context.Context, kubeClient client.Client, cloudProvider corecloudprovider.CloudProvider) (map[series]float64, map[series]float64, error) {
	nodePools := &karpv1.NodePoolList{}
	if err := kubeClient.List(ctx, nodePools); err != nil {
		return nil, nil, err
	}
	availability := map[series]bool{}
	price := map[series]float64{}
	var errs error
	for _, nodePool := range nodePools.Items {
		instanceTypes, err := cloudProvider.GetInstanceTypes(ctx, &nodePool)
		if err != nil {
			errs = multierr.Append(errs, err)
			continue
		}
		for _, instanceType := range instanceTypes {
			zones := sets.New[string]()
			for _, offering := range instanceType.Offerings {
				s := series{instanceType: instanceType.Name, capacityType: offering.CapacityType(), zone: offering.Zone()}
				availability[s] = availability[s] || offering.Available
				price[s] = offering.Price
				zones.Insert(offering.Zone())
			}
			if coreoptions.FromContext(ctx).FeatureGates.ReservedCapacity {
				for zone := range zones {
					s := series{instanceType: instanceType.Name, capacityType: karpv1.CapacityTypeReserved, zone: zone}
					if _, ok := availability[s]; !ok {
						availability[s] = false
						price[s] = 0
					}
				}
			}
		}
	}
	return lo.MapValues(availability, func(available bool, _ series) float64 { return lo.Ternary(available, 1.0, 0.0) }), price, errs
}

// gatherSeries returns every series currently registered for the named gauge.
func gatherSeries(name string) map[series]float64 {
	families := lo.Must(crmetrics.Registry.Gather())
	out := map[series]float64{}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, m := range family.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			out[series{instanceType: labels["instance_type"], capacityType: labels["capacity_type"], zone: labels["zone"]}] = m.GetGauge().GetValue()
		}
	}
	return out
}
