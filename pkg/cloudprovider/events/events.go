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

package events

import (
	"fmt"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
)

func NodePoolFailedToResolveNodeClass(nodePool *karpv1.NodePool) events.Event {
	return events.Event{
		InvolvedObject: nodePool,
		Type:           corev1.EventTypeWarning,
		Message:        "Failed resolving NodeClass",
		DedupeValues:   []string{string(nodePool.UID)},
	}
}

func NodePoolLikelyIncorrectRequirement(nodePool *karpv1.NodePool, key, hint string) events.Event {
	return events.Event{
		InvolvedObject: nodePool,
		Type:           corev1.EventTypeWarning,
		Reason:         "LikelyIncorrectRequirement",
		Message:        fmt.Sprintf("Karpenter doesn't use requirement %q to choose instances, it only sets it as a node label. %s", key, hint),
		DedupeValues:   []string{string(nodePool.UID), key},
	}
}

func NodeClaimFailedToResolveNodeClass(nodeClaim *karpv1.NodeClaim) events.Event {
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeWarning,
		Message:        "Failed resolving NodeClass",
		DedupeValues:   []string{string(nodeClaim.UID)},
	}
}

// NodePoolZonalShiftDetected is emitted when a zone the NodePool can provision into becomes shifted away from.
func NodePoolZonalShiftDetected(nodePool *karpv1.NodePool, zoneName, zoneID string) events.Event {
	return events.Event{
		InvolvedObject: nodePool,
		Type:           corev1.EventTypeWarning,
		Reason:         "ZonalShiftActive",
		Message:        fmt.Sprintf("Zonal shift detected: offerings in zone %s (%s) are unavailable for this NodePool", zoneName, zoneID),
		DedupeValues:   []string{string(nodePool.UID), zoneID},
	}
}

// NodePoolZonalShiftCleared is emitted when a previously shifted zone the NodePool can provision into is restored.
func NodePoolZonalShiftCleared(nodePool *karpv1.NodePool, zoneName, zoneID string) events.Event {
	return events.Event{
		InvolvedObject: nodePool,
		Type:           corev1.EventTypeNormal,
		Reason:         "ZonalShiftCleared",
		Message:        fmt.Sprintf("Zonal shift cleared: offerings in zone %s (%s) are restored for this NodePool", zoneName, zoneID),
		DedupeValues:   []string{string(nodePool.UID), zoneID},
	}
}

// NodeClaimInPlaceUpgradeFallback is emitted when a NodeClaim with AMI drift on a ReplaceRootVolume EC2NodeClass is
// replaced instead of having its root volume replaced in place. gate names the check that failed.
func NodeClaimInPlaceUpgradeFallback(nodeClaim *karpv1.NodeClaim, gate string) events.Event {
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeNormal,
		Reason:         "InPlaceUpgradeFallback",
		Message:        fmt.Sprintf("Replacing the node instead of its root volume, in-place check %s failed", gate),
		DedupeValues:   []string{string(nodeClaim.UID), gate},
	}
}

// NodeClaimInPlaceUpgradeRebootFallback is emitted when a drift reboot on a ReplaceRootVolume EC2NodeClass plainly
// reboots the drained node instead of replacing its root volume. cause names the failed gate or EC2's error code.
func NodeClaimInPlaceUpgradeRebootFallback(nodeClaim *karpv1.NodeClaim, cause string) events.Event {
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeWarning,
		Reason:         "InPlaceUpgradeRebootFallback",
		Message:        fmt.Sprintf("Rebooting the node without replacing its root volume (%s), drift will replace the node instead", cause),
		DedupeValues:   []string{string(nodeClaim.UID), cause},
	}
}

// EC2NodeClassInPlaceUpgradeIncomplete is emitted when nodes on an EC2NodeClass are replaced because the in-place
// checks for an AMI couldn't complete within the grace period. message names the check.
func EC2NodeClassInPlaceUpgradeIncomplete(nodeClass client.Object, amiID, message string) events.Event {
	return events.Event{
		InvolvedObject: nodeClass,
		Type:           corev1.EventTypeWarning,
		Reason:         "InPlaceUpgradeIncomplete",
		Message:        fmt.Sprintf("In-place upgrade checks for AMI %s couldn't complete (%s), replacing drifted nodes instead", amiID, lo.Ternary(message != "", message, "unknown check")),
		DedupeValues:   []string{string(nodeClass.GetUID()), amiID, message},
	}
}
