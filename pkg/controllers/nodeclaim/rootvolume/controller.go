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

package rootvolume

import (
	"context"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/utils/nodeclaim"

	"github.com/aws/karpenter-provider-aws/pkg/providers/rootvolume"
	"github.com/aws/karpenter-provider-aws/pkg/utils"
)

const (
	// Window is how long after the Rebooting condition goes terminal the controller keeps looking for the task
	// result. The reboot lifecycle can finish (new boot ID) before EC2 marks the task terminal.
	Window = 30 * time.Minute
	// PollInterval is how often an unfinished task is re-read.
	PollInterval = 10 * time.Second
)

// Controller writes status.imageID from the result of Karpenter's replace root volume task on the NodeClaim's
// instance. A swap changes the running image without a relaunch, and status.imageID feeds AMI drift and the
// discovered capacity cache. It reads the task provider only, never the instance cache, which does not expire and
// would serve the launch image.
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	clk           clock.Clock
	provider      rootvolume.Provider
}

func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, clk clock.Clock, provider rootvolume.Provider) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		clk:           clk,
		provider:      provider,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClaim *karpv1.NodeClaim) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodeclaim.rootvolume")
	// Requeues bypass the predicate, so check again; this is also where the window closes
	if !InScope(nodeClaim, c.clk.Now()) {
		return reconcile.Result{}, nil
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("Node", klog.KRef("", nodeClaim.Status.NodeName), "provider-id", nodeClaim.Status.ProviderID))
	id, err := utils.ParseInstanceID(nodeClaim.Status.ProviderID)
	if err != nil {
		// We don't throw an error here since we don't want to retry until the ProviderID has been updated.
		log.FromContext(ctx).Error(err, "failed parsing instance id")
		return reconcile.Result{}, nil
	}
	task, err := c.provider.Latest(ctx, id)
	if err != nil {
		return reconcile.Result{}, err
	}
	if task == nil {
		return reconcile.Result{}, nil
	}
	if !task.Terminal() {
		return reconcile.Result{RequeueAfter: PollInterval}, nil
	}
	if !task.Succeeded() || task.ImageID == "" || task.ImageID == nodeClaim.Status.ImageID {
		return reconcile.Result{}, nil
	}
	stored := nodeClaim.DeepCopy()
	nodeClaim.Status.ImageID = task.ImageID
	// A merge patch without an optimistic lock carries only status.imageID, so it cannot clobber the reboot
	// lifecycle's concurrent condition writes.
	if err := c.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	log.FromContext(ctx).WithValues("task-id", task.ID, "from-image-id", stored.Status.ImageID, "image-id", task.ImageID).Info("updated nodeclaim image from root volume replacement")
	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodeclaim.rootvolume").
		For(&karpv1.NodeClaim{}, builder.WithPredicates(nodeclaim.IsManagedPredicateFuncs(c.cloudProvider))).
		WithEventFilter(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return InScope(o.(*karpv1.NodeClaim), c.clk.Now())
		})).
		// Concurrent reconciles let the describe batcher coalesce lookups across NodeClaims
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

// InScope returns whether the NodeClaim may have a replace root volume task whose result is not yet reflected: it is
// launched, not deleting, and either rebooting or finished rebooting within Window. Any reboot reason qualifies, since
// the reboot lifecycle does not record which kind of reboot it ran.
func InScope(nc *karpv1.NodeClaim, now time.Time) bool {
	if !nc.DeletionTimestamp.IsZero() || nc.Status.ProviderID == "" || !nc.StatusConditions().Get(karpv1.ConditionTypeLaunched).IsTrue() {
		return false
	}
	cond := nc.StatusConditions().Get(karpv1.ConditionTypeRebooting)
	if cond == nil {
		return false
	}
	if cond.IsTrue() {
		return true
	}
	return cond.IsFalse() && now.Sub(cond.LastTransitionTime.Time) < Window
}
