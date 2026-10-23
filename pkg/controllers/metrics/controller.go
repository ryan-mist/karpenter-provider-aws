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

package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
)

type metricDimensions struct {
	instanceType string
	capacityType string
	zone         string
}

type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	// instanceTypeStore holds the NodeOverlay adjustments for each NodePool. It is applied per NodePool on top of
	// the instance types that cloudProvider resolves per NodeClass.
	instanceTypeStore *nodeoverlay.InstanceTypeStore
}

// NewController constructs the offering metrics controller. cloudProvider must not apply NodeOverlays itself: the
// controller resolves instance types once per NodeClass, which is the only input the AWS CloudProvider's
// GetInstanceTypes depends on, and then applies the per-NodePool overlays from instanceTypeStore, mirroring the
// overlay decorator. This keeps the emitted metrics identical to resolving every NodePool through the decorated
// CloudProvider, without repeating the instance type resolution for each NodePool that shares a NodeClass.
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, instanceTypeStore *nodeoverlay.InstanceTypeStore) *Controller {
	return &Controller{
		kubeClient:        kubeClient,
		cloudProvider:     cloudProvider,
		instanceTypeStore: instanceTypeStore,
	}
}

type instanceTypesResult struct {
	instanceTypes []*cloudprovider.InstanceType
	err           error
}

//nolint:gocyclo
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	nodePools := &karpv1.NodePoolList{}
	if err := c.kubeClient.List(ctx, nodePools); err != nil {
		return reconciler.Result{}, err
	}
	availability := map[metricDimensions]bool{}
	price := map[metricDimensions]float64{}
	// Instance types are resolved once per NodeClass and shared by every NodePool that references it. NodePools are
	// still visited in list order, so the per-NodePool overlay application and the last-write-wins price
	// aggregation below are unchanged.
	resolved := map[karpv1.NodeClassReference]instanceTypesResult{}
	var errs error
	for _, nodePool := range nodePools.Items {
		key := lo.FromPtr(nodePool.Spec.Template.Spec.NodeClassRef)
		result, ok := resolved[key]
		if !ok {
			result.instanceTypes, result.err = c.cloudProvider.GetInstanceTypes(ctx, &nodePool)
			resolved[key] = result
		}
		instanceTypes, err := result.instanceTypes, result.err
		if err == nil && coreoptions.FromContext(ctx).FeatureGates.NodeOverlay && c.instanceTypeStore != nil {
			if instanceTypes, err = c.instanceTypeStore.ApplyAll(nodePool.Name, instanceTypes); err != nil {
				err = fmt.Errorf("applying nodeoverlays, %w", err)
			}
		}
		if err != nil {
			// Keep going rather than abandoning the reconcile. A NodePool whose instance types can't be
			// resolved -- an undecodable spec.kubelet, for instance -- would otherwise suppress the
			// pricing and availability metrics of every other NodePool in the cluster, for as long as
			// that one NodePool stayed broken. The error is still returned below so the failure is
			// visible and retried.
			errs = multierr.Append(errs, serrors.Wrap(fmt.Errorf("getting instance types, %w", err), "NodePool", klog.KRef("", nodePool.Name)))
			continue
		}
		for _, instanceType := range instanceTypes {
			zones := sets.New[string]()
			for _, offering := range instanceType.Offerings {
				dimensions := metricDimensions{instanceType: instanceType.Name, capacityType: offering.CapacityType(), zone: offering.Zone()}
				availability[dimensions] = availability[dimensions] || offering.Available
				price[dimensions] = offering.Price
				zones.Insert(offering.Zone())
			}
			if coreoptions.FromContext(ctx).FeatureGates.ReservedCapacity {
				for zone := range zones {
					dimensions := metricDimensions{instanceType: instanceType.Name, capacityType: karpv1.CapacityTypeReserved, zone: zone}
					if _, ok := availability[dimensions]; !ok {
						availability[dimensions] = false
						price[dimensions] = 0
					}
				}
			}
		}
	}

	for dimensions, available := range availability {
		InstanceTypeOfferingAvailable.Set(float64(lo.Ternary(available, 1, 0)), map[string]string{
			instanceTypeLabel: dimensions.instanceType,
			capacityTypeLabel: dimensions.capacityType,
			zoneLabel:         dimensions.zone,
		})
	}
	for dimensions, p := range price {
		InstanceTypeOfferingPriceEstimate.Set(p, map[string]string{
			instanceTypeLabel: dimensions.instanceType,
			capacityTypeLabel: dimensions.capacityType,
			zoneLabel:         dimensions.zone,
		})
	}
	// Metrics for the NodePools that did resolve are published above before this returns, so one broken
	// NodePool costs only its own series rather than the whole cluster's.
	if errs != nil {
		return reconciler.Result{}, errs
	}
	return reconciler.Result{RequeueAfter: time.Minute}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("cloudprovider.metrics").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
