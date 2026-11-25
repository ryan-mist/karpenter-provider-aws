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

package cloudprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	cloudproviderevents "github.com/aws/karpenter-provider-aws/pkg/cloudprovider/events"
	awserrors "github.com/aws/karpenter-provider-aws/pkg/errors"
	"github.com/aws/karpenter-provider-aws/pkg/providers/amifamily"
	"github.com/aws/karpenter-provider-aws/pkg/providers/instance"
	"github.com/aws/karpenter-provider-aws/pkg/utils"
)

// Node-level gates for an in-place root volume replacement. Each names the check that sent a NodeClaim with AMI-only
// drift down the replace path. AMI-level gates are reported in status.amis[].inPlaceUpgrade and reuse its reasons.
const (
	InPlaceGateNotAMIOnly               = "NotAMIOnlyDrift"
	InPlaceGateNoTargetAMI              = "NoTargetAMI"
	InPlaceGatePreviousAttemptFailed    = "PreviousAttemptFailed"
	InPlaceGateInstanceNotRunning       = "InstanceNotRunning"
	InPlaceGateBareMetal                = "BareMetal"
	InPlaceGateInstanceProductCodes     = "InstanceProductCodes"
	InPlaceGateInstanceNitroTPM         = "InstanceNitroTPM"
	InPlaceGateBootMode                 = "BootMode"
	InPlaceGateENA                      = "ENA"
	InPlaceGateSriovNet                 = "SriovNet"
	InPlaceGateVariant                  = "Variant"
	InPlaceGateKubernetesVersion        = "KubernetesVersion"
	InPlaceGateKubernetesVersionUnknown = "KubernetesVersionUnknown"
	InPlaceGateTargetIsCurrentImage     = "TargetIsCurrentImage"
	// InPlaceGateChecksIncomplete is the gate when the target AMI's checks stayed Unknown past
	// InPlaceUpgradeUnknownGracePeriod.
	InPlaceGateChecksIncomplete = "InPlaceUpgradeUnknown"
)

const (
	sriovNetSupportSimple                = "simple"
	rootVolumeReplacementOperationPrefix = "replacing root volume"
)

// InPlaceUpgradeUnknownGracePeriod is how long nodes wait on a target AMI whose in-place checks couldn't complete,
// measured from when the AMI's status.amis result became Unknown. It covers transient EC2 errors, which the
// EC2NodeClass controller retries every minute. After it, the node is replaced so AMI drift isn't held forever.
const InPlaceUpgradeUnknownGracePeriod = 15 * time.Minute

// AMI variants, as named by amifamily.Variant.
var (
	inPlaceVariantNvidia   = string(amifamily.VariantNvidia)
	inPlaceVariantNeuron   = string(amifamily.VariantNeuron)
	inPlaceVariantStandard = string(amifamily.VariantStandard)
)

// deterministicCreateErrorCodes are CreateReplaceRootVolumeTask errors that won't succeed on retry, so the reboot
// fails right away instead of waiting out core's issuance timeout. Prefix matches cover the families EC2 returns.
var (
	deterministicCreateErrorCodes        = []string{awserrors.UnauthorizedOperationErrorCode, "InvalidSnapshot.NotFound", "OperationNotPermitted", "IdempotentParameterMismatch"}
	deterministicCreateErrorCodePrefixes = []string{"InvalidParameter", "InvalidAMIID."}
	// kubernetesMinorInName matches the Kubernetes version in EKS optimized and Bottlerocket AMI names, such as
	// amazon-eks-node-al2023-x86_64-standard-1.33-v20250101 and bottlerocket-aws-k8s-1.33-x86_64-v1.40.0-abc.
	kubernetesMinorInName = regexp.MustCompile(`(?:^|[-_])1\.(\d{1,3})(?:[-_]|$)`)
	kubeletMinor          = regexp.MustCompile(`^v?1\.(\d{1,3})(?:\.|$)`)
)

// rootVolumeToken derives the CreateReplaceRootVolumeTask client token from core's operationID. The operationID is
// longer than the 64 characters EC2 allows, and its SHA-256 hex digest is exactly 64 and stable across retries.
func rootVolumeToken(operationID string) string {
	sum := sha256.Sum256([]byte(operationID))
	return hex.EncodeToString(sum[:])
}

// inPlaceEvaluation is the outcome of evaluating a NodeClaim on a ReplaceRootVolume EC2NodeClass.
type inPlaceEvaluation struct {
	// reasons holds every drift reason, with AMIDrift last. It is empty when the NodeClaim has not drifted.
	reasons []cloudprovider.DriftReason
	// target is the AMI the node would move to. It is set when AMI drift is the only drift.
	target *v1.AMI
	// gate names the check that rules out an in-place replacement. It is empty when the node can move in place.
	gate string
	// joinImageID is set when this operation's task already exists, so Reboot adopts it instead of re-evaluating.
	joinImageID string
}

func (e inPlaceEvaluation) amiOnly() bool {
	return len(e.reasons) == 1 && e.reasons[0] == AMIDrift
}

func (c *CloudProvider) inPlaceDriftStatus(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1.EC2NodeClass) (cloudprovider.DriftStatus, error) {
	e, err := c.evaluateInPlace(ctx, nodeClaim, nodeClass, "")
	if err != nil {
		return cloudprovider.DriftStatus{}, err
	}
	if len(e.reasons) == 0 {
		return cloudprovider.NotDrifted, nil
	}
	if e.gate == "" {
		return cloudprovider.DriftStatus{Action: cloudprovider.RebootNode, Reason: AMIDrift}, nil
	}
	if lo.Contains(e.reasons, AMIDrift) {
		c.recorder.Publish(cloudproviderevents.NodeClaimInPlaceUpgradeFallback(nodeClaim, e.gate))
	}
	return cloudprovider.DriftStatus{Action: cloudprovider.ReplaceNode, Reason: e.reasons[0]}, nil
}

// evaluateInPlace computes every drift reason and, when AMI drift is the only one, whether the node can move to the
// target AMI in place. It makes no EC2 calls beyond the cached instance, cached image attributes and the root volume
// provider's task records. It returns an error when the answer has to wait: an in-place operation is still running,
// a check hasn't completed, or the instance is in a zonally shifted availability zone. Errors make core skip the
// NodeClaim for this pass rather than replace it.
//
// When token is set (from Reboot) and the instance's latest task carries it, the operation already created its task
// and evaluateInPlace returns joinImageID so the caller adopts it.
//
//nolint:gocyclo
func (c *CloudProvider) evaluateInPlace(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1.EC2NodeClass, token string) (inPlaceEvaluation, error) {
	if drifted := c.areStaticFieldsDrifted(nodeClaim, nodeClass); drifted != "" {
		return inPlaceEvaluation{reasons: []cloudprovider.DriftReason{drifted}, gate: InPlaceGateNotAMIOnly}, nil
	}
	instanceID, err := utils.ParseInstanceID(nodeClaim.Status.ProviderID)
	if err != nil {
		return inPlaceEvaluation{}, err
	}
	// The image the node runs. A succeeded task means the node already runs the task's image, even if
	// status.imageID hasn't caught up yet.
	imageID := nodeClaim.Status.ImageID
	var failedImageID string
	if nodeClaim.StatusConditions().Get(karpv1.ConditionTypeRebooting) != nil {
		task, err := c.rootVolumeProvider.Latest(ctx, instanceID)
		if err != nil {
			return inPlaceEvaluation{}, fmt.Errorf("getting root volume replacement task, %w", err)
		}
		switch {
		case task == nil:
		case token != "" && task.Token == token:
			return inPlaceEvaluation{joinImageID: task.ImageID}, nil
		case !task.Terminal():
			return inPlaceEvaluation{}, fmt.Errorf("root volume replacement task %s is %s", task.ID, task.State)
		case task.Succeeded() && task.ImageID != "":
			imageID = task.ImageID
		case task.Failed():
			failedImageID = task.ImageID
		}
	}
	target, err := c.targetAMI(ctx, nodeClaim, nodeClass)
	if err != nil {
		return inPlaceEvaluation{}, fmt.Errorf("calculating ami drift, %w", err)
	}
	if imageID == "" {
		return inPlaceEvaluation{}, fmt.Errorf("calculating ami drift, no ami ID found in nodeClaim status")
	}
	inst, err := c.getInstance(ctx, nodeClaim.Status.ProviderID)
	if err != nil {
		return inPlaceEvaluation{}, err
	}
	securitygroupDrifted, err := c.areSecurityGroupsDrifted(inst, nodeClass)
	if err != nil {
		return inPlaceEvaluation{}, fmt.Errorf("calculating securitygroup drift, %w", err)
	}
	subnetDrifted, err := c.isSubnetDrifted(inst, nodeClass)
	if err != nil {
		return inPlaceEvaluation{}, fmt.Errorf("calculating subnet drift, %w", err)
	}
	capacityReservationsDrifted, err := c.isCapacityReservationDrifted(ctx, nodeClaim.Status.ProviderID, inst, nodeClass)
	if err != nil {
		return inPlaceEvaluation{}, fmt.Errorf("calculating capacity reservation drift, %w", err)
	}
	placementGroupDrifted := c.isPlacementGroupDrifted(ctx, nodeClaim, nodeClass)
	e := inPlaceEvaluation{reasons: lo.Filter([]cloudprovider.DriftReason{
		securitygroupDrifted,
		subnetDrifted,
		capacityReservationsDrifted,
		placementGroupDrifted,
	}, func(r cloudprovider.DriftReason, _ int) bool { return r != "" })}
	if target == nil || target.ID != imageID {
		e.reasons = append(e.reasons, AMIDrift)
	}
	switch {
	case len(e.reasons) == 0:
		return e, nil
	case !e.amiOnly():
		e.gate = InPlaceGateNotAMIOnly
		return e, nil
	case target == nil:
		e.gate = InPlaceGateNoTargetAMI
		return e, nil
	case failedImageID != "" && failedImageID == target.ID:
		// Karpenter's last attempt toward this exact target failed. Retrying in place would likely fail the same way.
		e.gate = InPlaceGatePreviousAttemptFailed
		return e, nil
	}
	e.target = target
	if c.instanceProvider.IsZonalShifted(ctx, inst) {
		return inPlaceEvaluation{}, fmt.Errorf("instance %s is in zonally shifted availability zone %s (%s), skipping in-place drift", inst.ID, inst.Zone, inst.ZoneID)
	}
	e.gate, err = c.inPlaceNodeGate(ctx, nodeClaim, nodeClass, inst, target, imageID)
	if err != nil {
		return inPlaceEvaluation{}, err
	}
	return e, nil
}

// inPlaceNodeGate checks the target AMI's status.amis result and the node-level requirements EC2 places on replacing
// a root volume from an AMI. It returns the first failed gate, or "" when every gate passes.
//
//nolint:gocyclo
func (c *CloudProvider) inPlaceNodeGate(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1.EC2NodeClass, inst *instance.Instance, target *v1.AMI, currentImageID string) (string, error) {
	switch {
	case target.InPlaceUpgrade == nil:
		// The EC2NodeClass controller fills in every entry of a ReplaceRootVolume EC2NodeClass, as Unknown when a check
		// fails, in the same reconcile that writes it. A missing result only lasts until that reconcile.
		return "", fmt.Errorf("in-place upgrade checks for ami %s haven't run yet", target.ID)
	case !target.InPlaceUpgrade.Supported && target.InPlaceUpgrade.Reason == v1.InPlaceUpgradeReasonUnknown:
		if since := target.InPlaceUpgrade.LastTransitionTime; since != nil && c.clk.Since(since.Time) < InPlaceUpgradeUnknownGracePeriod {
			return "", fmt.Errorf("in-place upgrade checks for ami %s couldn't complete (%s)", target.ID, target.InPlaceUpgrade.Message)
		}
		c.recorder.Publish(cloudproviderevents.EC2NodeClassInPlaceUpgradeIncomplete(nodeClass, target.ID, target.InPlaceUpgrade.Message))
		return InPlaceGateChecksIncomplete, nil
	case !target.InPlaceUpgrade.Supported:
		return target.InPlaceUpgrade.Reason, nil
	case target.ID == currentImageID:
		return InPlaceGateTargetIsCurrentImage, nil
	case inst.State != ec2types.InstanceStateNameRunning:
		return InPlaceGateInstanceNotRunning, nil
	case isBareMetal(inst.Type):
		return InPlaceGateBareMetal, nil
	case len(inst.ProductCodes) > 0:
		return InPlaceGateInstanceProductCodes, nil
	}
	images, err := c.amiProvider.Images(ctx, target.ID, currentImageID)
	if err != nil {
		return "", fmt.Errorf("getting image attributes, %w", err)
	}
	targetImage, ok := images[target.ID]
	if !ok {
		return "", fmt.Errorf("getting image attributes, ami %s not found", target.ID)
	}
	currentImage := images[currentImageID]
	switch {
	// EC2 keeps the instance's boot mode. An AMI that requires uefi or legacy-bios must match it; uefi-preferred or
	// an unset boot mode boots in the instance's current mode.
	case (targetImage.BootMode == ec2types.BootModeValuesUefi || targetImage.BootMode == ec2types.BootModeValuesLegacyBios) &&
		string(targetImage.BootMode) != string(inst.BootMode):
		return InPlaceGateBootMode, nil
	// EC2 refuses to replace the root volume of a NitroTPM instance from an AMI without NitroTPM. Every other
	// combination is allowed.
	case inst.TpmSupport != "" && targetImage.TpmSupport != ec2types.TpmSupportValuesV20:
		return InPlaceGateInstanceNitroTPM, nil
	case inst.EnaSupport && !targetImage.EnaSupport:
		return InPlaceGateENA, nil
	case inst.SriovNetSupport == sriovNetSupportSimple && targetImage.SriovNetSupport != sriovNetSupportSimple:
		return InPlaceGateSriovNet, nil
	case c.currentVariant(ctx, nodeClaim, nodeClass, currentImageID, currentImage) != variantOf(target.Requirements, target.Name):
		return InPlaceGateVariant, nil
	}
	currentMinor := c.currentKubernetesMinor(ctx, nodeClaim, currentImage)
	targetMinor := kubernetesMinorFromName(lo.Ternary(target.Name != "", target.Name, targetImage.Name))
	switch {
	case currentMinor == "" || targetMinor == "":
		return InPlaceGateKubernetesVersionUnknown, nil
	case currentMinor != targetMinor:
		return InPlaceGateKubernetesVersion, nil
	}
	return "", nil
}

// currentVariant returns the variant (standard, nvidia or neuron) of the image the node runs. It prefers the image's
// status.amis entry for the node's instance type, then the image name, then the instance type's accelerators when the
// image can no longer be described.
func (c *CloudProvider) currentVariant(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1.EC2NodeClass, imageID string, image *amifamily.Image) string {
	entries := lo.Filter(nodeClass.Status.AMIs, func(ami v1.AMI, _ int) bool { return ami.ID == imageID })
	if len(entries) > 0 {
		if it, err := c.instanceTypeProvider.Get(ctx, nodeClass, ec2types.InstanceType(nodeClaim.Labels[corev1.LabelInstanceTypeStable])); err == nil {
			if mapped := amifamily.MapToInstanceTypes([]*cloudprovider.InstanceType{it}, entries); len(mapped) == 1 {
				if entry, ok := lo.Find(entries, func(ami v1.AMI) bool {
					return it.Requirements.Compatible(scheduling.NewNodeSelectorRequirements(ami.Requirements...)) == nil
				}); ok {
					return variantOf(entry.Requirements, entry.Name)
				}
			}
		}
	}
	if image != nil {
		return variantOf(nil, image.Name)
	}
	switch {
	case nodeClaim.Labels[v1.LabelInstanceGPUCount] != "" && nodeClaim.Labels[v1.LabelInstanceGPUCount] != "0":
		return inPlaceVariantNvidia
	case nodeClaim.Labels[v1.LabelInstanceAcceleratorCount] != "" && nodeClaim.Labels[v1.LabelInstanceAcceleratorCount] != "0":
		return inPlaceVariantNeuron
	default:
		return inPlaceVariantStandard
	}
}

// variantOf derives an AMI's variant from the requirements Karpenter attaches to alias AMIs, falling back to the
// tokens EKS optimized and Bottlerocket AMI names carry.
func variantOf(requirements []corev1.NodeSelectorRequirement, name string) string {
	gpu, gpuOK := lo.Find(requirements, func(r corev1.NodeSelectorRequirement) bool { return r.Key == v1.LabelInstanceGPUCount })
	accel, accelOK := lo.Find(requirements, func(r corev1.NodeSelectorRequirement) bool {
		return r.Key == v1.LabelInstanceAcceleratorCount
	})
	switch {
	case gpuOK && gpu.Operator == corev1.NodeSelectorOpExists:
		return inPlaceVariantNvidia
	case accelOK && accel.Operator == corev1.NodeSelectorOpExists:
		return inPlaceVariantNeuron
	case gpuOK && accelOK && gpu.Operator == corev1.NodeSelectorOpDoesNotExist && accel.Operator == corev1.NodeSelectorOpDoesNotExist:
		return inPlaceVariantStandard
	}
	name = strings.ToLower(name)
	switch {
	case strings.Contains(name, "neuron"):
		return inPlaceVariantNeuron
	case strings.Contains(name, "nvidia"), strings.Contains(name, "gpu"):
		return inPlaceVariantNvidia
	default:
		return inPlaceVariantStandard
	}
}

// currentKubernetesMinor returns the node's Kubernetes minor version, from the kubelet the Node reports or else from
// the current image's name. It returns "" when neither is known.
func (c *CloudProvider) currentKubernetesMinor(ctx context.Context, nodeClaim *karpv1.NodeClaim, image *amifamily.Image) string {
	if nodeClaim.Status.NodeName != "" {
		node := &corev1.Node{}
		if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodeClaim.Status.NodeName}, node); err == nil {
			if m := kubeletMinor.FindStringSubmatch(node.Status.NodeInfo.KubeletVersion); m != nil {
				return m[1]
			}
		} else if client.IgnoreNotFound(err) != nil {
			log.FromContext(ctx).V(1).Error(err, "failed getting node for in-place drift")
		}
	}
	if image != nil {
		return kubernetesMinorFromName(image.Name)
	}
	return ""
}

func kubernetesMinorFromName(name string) string {
	if m := kubernetesMinorInName.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

// isBareMetal reports whether EC2 refuses root volume replacement for the instance type. Mac bare metal is the
// only bare metal EC2 supports.
func isBareMetal(instanceType ec2types.InstanceType) bool {
	name := string(instanceType)
	return strings.Contains(name, ".metal") && !strings.HasPrefix(name, "mac")
}

func disruptionReason(nodeClaim *karpv1.NodeClaim) karpv1.DisruptionReason {
	if cond := nodeClaim.StatusConditions().Get(karpv1.ConditionTypeDisruptionReason); cond.IsTrue() {
		return karpv1.DisruptionReason(cond.Reason)
	}
	return ""
}

// replaceRootVolume issues a drift reboot on a ReplaceRootVolume EC2NodeClass. The gates are re-evaluated from memory
// because the target AMI or the node may have changed since the reboot was committed.
func (c *CloudProvider) replaceRootVolume(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1.EC2NodeClass, instanceID, operationID string) error {
	token := rootVolumeToken(operationID)
	e, err := c.evaluateInPlace(ctx, nodeClaim, nodeClass, token)
	if err != nil {
		return fmt.Errorf("%s, %w", rootVolumeReplacementOperationPrefix, err)
	}
	imageID := e.joinImageID
	switch {
	case imageID != "":
	case len(e.reasons) == 0:
		// The drift resolved itself after the commit, for example because the EC2NodeClass was reverted.
		return c.instanceProvider.Reboot(ctx, instanceID, operationID)
	case e.gate != "":
		return cloudprovider.NewNodeRebootFailedError(fmt.Errorf("%s, in-place replacement is no longer allowed (%s)", rootVolumeReplacementOperationPrefix, e.gate))
	default:
		imageID = e.target.ID
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("image-id", imageID, "operation-id", operationID))
	task, err := c.rootVolumeProvider.Create(ctx, instanceID, imageID, token)
	if err != nil {
		return classifyCreateError(err)
	}
	log.FromContext(ctx).V(1).WithValues("task-id", task.ID, "state", task.State).Info("replacing root volume")
	return nil
}

// classifyCreateError fails the reboot for errors retrying can't fix, and leaves everything else, such as throttling,
// server errors and a task already running on the instance, to core's retry with the same token.
func classifyCreateError(err error) error {
	wrapped := fmt.Errorf("%s, %w", rootVolumeReplacementOperationPrefix, err)
	if awserrors.IsNotFound(err) {
		return cloudprovider.NewNodeClaimNotFoundError(wrapped)
	}
	var apiErr smithy.APIError
	if !stderrors.As(err, &apiErr) {
		return wrapped
	}
	code := apiErr.ErrorCode()
	if lo.Contains(deterministicCreateErrorCodes, code) || lo.SomeBy(deterministicCreateErrorCodePrefixes, func(p string) bool { return strings.HasPrefix(code, p) }) {
		return cloudprovider.NewNodeRebootFailedError(wrapped)
	}
	return wrapped
}
