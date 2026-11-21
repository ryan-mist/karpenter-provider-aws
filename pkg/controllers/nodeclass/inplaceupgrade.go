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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/providers/amifamily"
)

const (
	// snapshotReadableTTL keeps snapshot permission results for about one AMI reconcile period, so sharing changes
	// made outside Karpenter are picked up on the next pass.
	snapshotReadableTTL = 5 * time.Minute
	// inPlaceUpgradeRequeue is how soon checks that could not complete are retried.
	inPlaceUpgradeRequeue = time.Minute
)

// Messages for an Unknown result, naming the check that couldn't complete. They are fixed strings so a repeated
// failure doesn't rewrite the status.
const (
	inPlaceUpgradeMessageImage       = "describing the AMI failed"
	inPlaceUpgradeMessageImageAbsent = "the AMI could not be found"
	inPlaceUpgradeMessageSnapshot    = "checking the root snapshot's permissions failed"
)

// SnapshotAPI is the EC2 call the snapshot check needs.
type SnapshotAPI interface {
	DescribeSnapshots(context.Context, *ec2.DescribeSnapshotsInput, ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)
}

// InPlaceUpgrade fills status.amis[].inPlaceUpgrade for EC2NodeClasses with amiDriftPolicy ReplaceRootVolume. It runs
// right after the AMI reconciler and never fails the EC2NodeClass reconcile: a check that can't complete marks the
// AMI with reason Unknown, which holds AMI drift on that AMI instead of replacing nodes.
type InPlaceUpgrade struct {
	clk           clock.Clock
	amiProvider   amifamily.Provider
	snapshotAPI   SnapshotAPI
	snapshotCache *cache.Cache
}

// NewInPlaceUpgradeReconciler builds the reconciler. snapshotAPI may be nil, in which case every private AMI is
// reported Unknown.
func NewInPlaceUpgradeReconciler(clk clock.Clock, amiProvider amifamily.Provider, snapshotAPI SnapshotAPI) *InPlaceUpgrade {
	return &InPlaceUpgrade{
		clk:           clk,
		amiProvider:   amiProvider,
		snapshotAPI:   snapshotAPI,
		snapshotCache: cache.New(snapshotReadableTTL, time.Minute),
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
		case image.TpmSupport == ec2types.TpmSupportValuesV20:
			unsupported(id, v1.InPlaceUpgradeReasonNitroTPM)
		case image.RootSnapshotID == "":
			unsupported(id, v1.InPlaceUpgradeReasonSnapshotNotReadable)
		case image.Public:
			// RunInstances and CreateReplaceRootVolumeTask both work from public AMIs whose snapshots aren't shared
			// with the account, as the public EKS optimized AMIs are, so the snapshot check only covers private AMIs.
			results[id] = &v1.InPlaceUpgrade{Supported: true}
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
	u.apply(nodeClass, results)
	if requeue {
		return reconcile.Result{RequeueAfter: inPlaceUpgradeRequeue}, nil
	}
	return reconcile.Result{}, nil
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
	found := map[string]bool{}
	// "self" covers snapshots the account owns or that were shared with it; "all" covers public snapshots. They are
	// separate calls because the API doesn't document how several restorable-by values combine.
	for _, restorableBy := range []string{"self", "all"} {
		remaining := lo.Reject(misses, func(id string, _ int) bool { return found[id] })
		for _, chunk := range lo.Chunk(remaining, 200) {
			paginator := ec2.NewDescribeSnapshotsPaginator(u.snapshotAPI, &ec2.DescribeSnapshotsInput{
				Filters:             []ec2types.Filter{{Name: aws.String("snapshot-id"), Values: chunk}},
				RestorableByUserIds: []string{restorableBy},
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
	}
	for _, id := range misses {
		out[id] = found[id]
		u.snapshotCache.SetDefault(id, found[id])
	}
	return out, nil
}
