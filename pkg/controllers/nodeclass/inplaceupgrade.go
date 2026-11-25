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

package nodeclass

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	awserrors "github.com/aws/karpenter-provider-aws/pkg/errors"
	"github.com/aws/karpenter-provider-aws/pkg/operator/options"
	"github.com/aws/karpenter-provider-aws/pkg/providers/amifamily"
	"github.com/aws/karpenter-provider-aws/pkg/providers/rootvolume"
	"github.com/aws/karpenter-provider-aws/pkg/utils"
)

const (
	// snapshotReadableTTL keeps snapshot permission results for about one AMI reconcile period, so sharing changes
	// made outside Karpenter are picked up on the next pass.
	snapshotReadableTTL = 5 * time.Minute
	// dryRunTTL keeps permission dry run results for about one AMI reconcile period, so a role change is picked up on
	// the next pass. It is measured on the reconciler's clock; the cache's own expiry only cleans up.
	dryRunTTL = 5 * time.Minute
	// inPlaceUpgradeRequeue is how soon checks that could not complete are retried.
	inPlaceUpgradeRequeue = time.Minute
)

// Messages for an Unknown result, naming the check that couldn't complete. They are fixed strings so a repeated
// failure doesn't rewrite the status.
const (
	inPlaceUpgradeMessageImage       = "describing the AMI failed"
	inPlaceUpgradeMessageImageAbsent = "the AMI could not be found"
	inPlaceUpgradeMessageSnapshot    = "checking the root snapshot's permissions failed"
	inPlaceUpgradeMessageDryRun      = "the CreateReplaceRootVolumeTask dry run failed"
	// inPlaceUpgradeMessageNoInstance is reported while the EC2NodeClass has no running node to run the dry run
	// against. No node can drift then, so nothing waits on it.
	inPlaceUpgradeMessageNoInstance = "waiting for a running node to check permissions against"
)

// deniedActionPattern finds the action EC2 names in an UnauthorizedOperation message.
var deniedActionPattern = regexp.MustCompile(`not authorized to perform:? (ec2:[A-Za-z]+)`)

// dryRunResult is a deterministic dry run outcome. Transient failures are not cached.
type dryRunResult struct {
	allowed      bool
	deniedAction string
	at           time.Time
}

// SnapshotAPI is the EC2 call the snapshot check needs.
type SnapshotAPI interface {
	DescribeSnapshots(context.Context, *ec2.DescribeSnapshotsInput, ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)
}

// InPlaceUpgrade fills status.amis[].inPlaceUpgrade for EC2NodeClasses with amiDriftPolicy ReplaceRootVolume. It runs
// right after the AMI reconciler and never fails the EC2NodeClass reconcile: a check that can't complete marks the
// AMI with reason Unknown, which holds AMI drift on that AMI instead of replacing nodes.
type InPlaceUpgrade struct {
	clk                clock.Clock
	kubeClient         client.Client
	amiProvider        amifamily.Provider
	snapshotAPI        SnapshotAPI
	rootVolumeProvider rootvolume.Provider
	snapshotCache      *cache.Cache
	dryRunCache        *cache.Cache
}

// NewInPlaceUpgradeReconciler builds the reconciler. snapshotAPI may be nil, in which case every AMI is
// reported Unknown. rootVolumeProvider may be nil, in which case the permission dry run is skipped; the cloud provider
// never replaces root volumes without one.
func NewInPlaceUpgradeReconciler(clk clock.Clock, kubeClient client.Client, amiProvider amifamily.Provider, snapshotAPI SnapshotAPI, rootVolumeProvider rootvolume.Provider) *InPlaceUpgrade {
	return &InPlaceUpgrade{
		clk:                clk,
		kubeClient:         kubeClient,
		amiProvider:        amiProvider,
		snapshotAPI:        snapshotAPI,
		rootVolumeProvider: rootVolumeProvider,
		snapshotCache:      cache.New(snapshotReadableTTL, time.Minute),
		dryRunCache:        cache.New(time.Hour, time.Hour),
	}
}

//nolint:gocyclo
func (u *InPlaceUpgrade) Reconcile(ctx context.Context, nodeClass *v1.EC2NodeClass) (reconcile.Result, error) {
	if !nodeClass.ReplacesRootVolumeOnAMIDrift() {
		for i := range nodeClass.Status.AMIs {
			nodeClass.Status.AMIs[i].InPlaceUpgrade = nil
		}
		return reconcile.Result{}, nil
	}
	if len(nodeClass.Status.AMIs) == 0 {
		return reconcile.Result{}, nil
	}
	results := map[string]*v1.InPlaceUpgrade{}
	unsupported := func(id, reason string) { results[id] = &v1.InPlaceUpgrade{Supported: false, Reason: reason} }
	unknown := func(id, message string) {
		results[id] = &v1.InPlaceUpgrade{Supported: false, Reason: v1.InPlaceUpgradeReasonUnknown, Message: message}
	}
	ids := lo.Uniq(lo.Map(nodeClass.Status.AMIs, func(ami v1.AMI, _ int) string { return ami.ID }))

	switch family := nodeClass.AMIFamily(); family {
	case v1.AMIFamilyCustom, v1.AMIFamilyWindows2019, v1.AMIFamilyWindows2022, v1.AMIFamilyWindows2025:
		for _, id := range ids {
			unsupported(id, v1.InPlaceUpgradeReasonUnsupportedAMIFamily)
		}
		u.apply(nodeClass, results)
		return reconcile.Result{}, nil
	}

	var requeue bool
	imageMessage := inPlaceUpgradeMessageImageAbsent
	images, err := u.amiProvider.Images(ctx, ids...)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed checking amis for in-place upgrade")
		images, requeue, imageMessage = map[string]*amifamily.Image{}, true, inPlaceUpgradeMessageImage
	}
	needSnapshot := map[string]string{}
	for _, id := range ids {
		image, ok := images[id]
		switch {
		case !ok:
			unknown(id, imageMessage)
			requeue = true
		case image.Platform == ec2types.PlatformValuesWindows:
			unsupported(id, v1.InPlaceUpgradeReasonUnsupportedAMIFamily)
		case len(image.ProductCodes) > 0:
			unsupported(id, v1.InPlaceUpgradeReasonProductCodes)
		case image.RootSnapshotID == "":
			unsupported(id, v1.InPlaceUpgradeReasonSnapshotNotReadable)
		default:
			needSnapshot[id] = image.RootSnapshotID
		}
	}
	if len(needSnapshot) > 0 {
		readable, err := u.readableSnapshots(ctx, lo.Uniq(lo.Values(needSnapshot)))
		if err != nil {
			log.FromContext(ctx).Error(err, "failed checking ami snapshots for in-place upgrade")
			requeue = true
		}
		for id, snapshotID := range needSnapshot {
			switch r, ok := readable[snapshotID]; {
			case !ok:
				unknown(id, inPlaceUpgradeMessageSnapshot)
			case !r:
				unsupported(id, v1.InPlaceUpgradeReasonSnapshotNotReadable)
			default:
				results[id] = &v1.InPlaceUpgrade{Supported: true}
			}
		}
	}
	if u.checkPermissions(ctx, nodeClass, results) {
		requeue = true
	}
	u.apply(nodeClass, results)
	if requeue {
		return reconcile.Result{RequeueAfter: inPlaceUpgradeRequeue}, nil
	}
	return reconcile.Result{}, nil
}

// checkPermissions runs a DryRun CreateReplaceRootVolumeTask toward each AMI that passed every other check, against
// one of the EC2NodeClass's running instances, so a role without the permissions the swap needs replaces nodes
// instead of draining them for a swap EC2 will reject. It updates results in place and reports whether to requeue.
func (u *InPlaceUpgrade) checkPermissions(ctx context.Context, nodeClass *v1.EC2NodeClass, results map[string]*v1.InPlaceUpgrade) bool {
	if u.rootVolumeProvider == nil || options.FromContext(ctx).DisableDryRun {
		return false
	}
	var pending []string
	for id, result := range results {
		if !result.Supported {
			continue
		}
		if v, ok := u.dryRunCache.Get(dryRunCacheKey(nodeClass, id)); ok && u.clk.Since(v.(dryRunResult).at) < dryRunTTL {
			applyDryRun(results, id, v.(dryRunResult))
			continue
		}
		pending = append(pending, id)
	}
	if len(pending) == 0 {
		return false
	}
	instanceID, err := u.dryRunInstance(ctx, nodeClass)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed finding an instance for the in-place upgrade dry run")
		for _, id := range pending {
			results[id] = &v1.InPlaceUpgrade{Supported: false, Reason: v1.InPlaceUpgradeReasonUnknown, Message: inPlaceUpgradeMessageDryRun}
		}
		return true
	}
	if instanceID == "" {
		// Without a running node there is nothing to drift, so the check waits for the next reconcile.
		for _, id := range pending {
			results[id] = &v1.InPlaceUpgrade{Supported: false, Reason: v1.InPlaceUpgradeReasonUnknown, Message: inPlaceUpgradeMessageNoInstance}
		}
		return false
	}
	var requeue bool
	for _, id := range pending {
		err := u.rootVolumeProvider.DryRun(ctx, instanceID, id)
		var result dryRunResult
		switch {
		case err == nil:
			result = dryRunResult{allowed: true, at: u.clk.Now()}
		case awserrors.IsUnauthorizedOperationError(err):
			result = dryRunResult{deniedAction: deniedAction(err), at: u.clk.Now()}
		default:
			// Throttling, a task already running on the instance, server errors and the like. The AMI waits out the
			// Unknown grace period while this retries.
			log.FromContext(ctx).WithValues("instance-id", instanceID, "image-id", id).Error(err, "failed in-place upgrade dry run")
			results[id] = &v1.InPlaceUpgrade{Supported: false, Reason: v1.InPlaceUpgradeReasonUnknown, Message: inPlaceUpgradeMessageDryRun}
			requeue = true
			continue
		}
		u.dryRunCache.SetDefault(dryRunCacheKey(nodeClass, id), result)
		applyDryRun(results, id, result)
	}
	return requeue
}

func applyDryRun(results map[string]*v1.InPlaceUpgrade, id string, result dryRunResult) {
	if result.allowed {
		return
	}
	results[id] = &v1.InPlaceUpgrade{
		Supported: false,
		Reason:    v1.InPlaceUpgradeReasonPermissionDenied,
		Message:   fmt.Sprintf("not authorized to perform %s", result.deniedAction),
	}
}

// dryRunInstance picks a running instance of the EC2NodeClass from the cached NodeClaims, preferring the oldest. It
// skips NodeClaims that are rebooting, since EC2 rejects the call while a task runs on the instance. It returns ""
// when there is none.
func (u *InPlaceUpgrade) dryRunInstance(ctx context.Context, nodeClass *v1.EC2NodeClass) (string, error) {
	nodeClaims := &karpv1.NodeClaimList{}
	if err := u.kubeClient.List(ctx, nodeClaims, nodeclaimutils.ForNodeClass(nodeClass)); err != nil {
		return "", fmt.Errorf("listing nodeclaims, %w", err)
	}
	candidates := lo.Filter(nodeClaims.Items, func(nc karpv1.NodeClaim, _ int) bool {
		return nc.DeletionTimestamp.IsZero() && nc.Status.ProviderID != "" &&
			nc.StatusConditions().Get(karpv1.ConditionTypeInitialized).IsTrue() &&
			!nc.StatusConditions().Get(karpv1.ConditionTypeRebooting).IsTrue()
	})
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].CreationTimestamp.Equal(&candidates[j].CreationTimestamp) {
			return candidates[i].CreationTimestamp.Before(&candidates[j].CreationTimestamp)
		}
		return candidates[i].Name < candidates[j].Name
	})
	for _, nc := range candidates {
		if id, err := utils.ParseInstanceID(nc.Status.ProviderID); err == nil {
			return id, nil
		}
	}
	return "", nil
}

func dryRunCacheKey(nodeClass *v1.EC2NodeClass, imageID string) string {
	return fmt.Sprintf("%s/%s", nodeClass.UID, imageID)
}

// deniedAction names the action EC2 reported as denied, defaulting to CreateReplaceRootVolumeTask. It is kept to the
// action alone, without the resource, so the status message stays the same across instances.
func deniedAction(err error) string {
	if apiErr, ok := lo.ErrorsAs[smithy.APIError](err); ok {
		if m := deniedActionPattern.FindStringSubmatch(apiErr.ErrorMessage()); m != nil {
			return m[1]
		}
	}
	return "ec2:CreateReplaceRootVolumeTask"
}

func (u *InPlaceUpgrade) apply(nodeClass *v1.EC2NodeClass, results map[string]*v1.InPlaceUpgrade) {
	for i := range nodeClass.Status.AMIs {
		current, result := nodeClass.Status.AMIs[i].InPlaceUpgrade, results[nodeClass.Status.AMIs[i].ID]
		// Only write when the value changes, so an unchanged result never produces a status patch and keeps the time
		// it was first reported.
		if result == nil || current == nil || current.Supported != result.Supported || current.Reason != result.Reason || current.Message != result.Message {
			if result != nil {
				result = result.DeepCopy()
				result.LastTransitionTime = lo.ToPtr(metav1.NewTime(u.clk.Now()))
			}
			nodeClass.Status.AMIs[i].InPlaceUpgrade = result
		}
	}
}

// readableSnapshots reports, for each snapshot it could check, whether the account can create a volume from it.
// Snapshots missing from the result could not be checked.
func (u *InPlaceUpgrade) readableSnapshots(ctx context.Context, snapshotIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	var misses []string
	for _, id := range snapshotIDs {
		if v, ok := u.snapshotCache.Get(id); ok {
			out[id] = v.(bool)
			continue
		}
		misses = append(misses, id)
	}
	if len(misses) == 0 {
		return out, nil
	}
	if u.snapshotAPI == nil {
		return out, fmt.Errorf("describing snapshots, no snapshot API configured")
	}
	// Without owner or restorable-by parameters, DescribeSnapshots returns every snapshot the account can create a
	// volume from: its own, those shared with it and public ones. The snapshot-id filter drops the ones it can't
	// read from the result, where listing them in SnapshotIds would fail the whole call.
	found := map[string]bool{}
	for _, chunk := range lo.Chunk(misses, 200) {
		paginator := ec2.NewDescribeSnapshotsPaginator(u.snapshotAPI, &ec2.DescribeSnapshotsInput{
			Filters: []ec2types.Filter{{Name: aws.String("snapshot-id"), Values: chunk}},
		})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return out, fmt.Errorf("describing snapshots, %w", err)
			}
			for _, snapshot := range page.Snapshots {
				found[lo.FromPtr(snapshot.SnapshotId)] = true
			}
		}
	}
	for _, id := range misses {
		out[id] = found[id]
		u.snapshotCache.SetDefault(id, found[id])
	}
	return out, nil
}
