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

package errors_test

import (
	"errors"
	"fmt"
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	"go.uber.org/multierr"

	awserrors "github.com/aws/karpenter-provider-aws/pkg/errors"
)

func TestErrors(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Errors")
}

func apiError(code, message string) error {
	return &smithy.GenericAPIError{Code: code, Message: message}
}

func fleetError(code, message string) ec2types.CreateFleetError {
	return ec2types.CreateFleetError{ErrorCode: lo.ToPtr(code), ErrorMessage: lo.ToPtr(message)}
}

var _ = Describe("Errors", func() {
	Context("API error predicates", func() {
		It("should match not found codes through wrapping", func() {
			for _, code := range []string{
				"InvalidCapacityReservationId.NotFound",
				"InvalidInstanceID.NotFound",
				"InvalidLaunchTemplateName.NotFoundException",
				"InvalidLaunchTemplateId.NotFound",
				"QueueDoesNotExist",
				"NoSuchEntity",
				"ParameterNotFound",
			} {
				err := fmt.Errorf("describing resource, %w", apiError(code, "not found"))
				Expect(awserrors.IsNotFound(err)).To(BeTrue(), code)
				Expect(awserrors.IgnoreNotFound(err)).To(Succeed(), code)
			}
		})
		It("should not treat other errors as not found", func() {
			Expect(awserrors.IsNotFound(nil)).To(BeFalse())
			// The code has to come from the API error, not the message
			Expect(awserrors.IsNotFound(errors.New("InvalidInstanceID.NotFound"))).To(BeFalse())
			err := apiError("InvalidInstanceID.Malformed", "malformed")
			Expect(awserrors.IsNotFound(err)).To(BeFalse())
			Expect(awserrors.IgnoreNotFound(err)).To(MatchError(err))
		})
		It("should match already exists codes through wrapping", func() {
			for _, code := range []string{"EntityAlreadyExists", "InvalidLaunchTemplateName.AlreadyExistsException"} {
				err := fmt.Errorf("creating resource, %w", apiError(code, "exists"))
				Expect(awserrors.IsAlreadyExists(err)).To(BeTrue(), code)
				Expect(awserrors.IgnoreAlreadyExists(err)).To(Succeed(), code)
			}
			Expect(awserrors.IsAlreadyExists(nil)).To(BeFalse())
			err := apiError("NoSuchEntity", "missing")
			Expect(awserrors.IsAlreadyExists(err)).To(BeFalse())
			Expect(awserrors.IgnoreAlreadyExists(err)).To(MatchError(err))
		})
		DescribeTable("should match exactly one error code",
			func(is func(error) bool, ignore func(error) error, code string) {
				err := fmt.Errorf("wrapped, %w", apiError(code, "message"))
				Expect(is(err)).To(BeTrue())
				Expect(ignore(err)).To(Succeed())
				Expect(is(nil)).To(BeFalse())
				other := apiError("SomeOtherCode", "message")
				Expect(is(other)).To(BeFalse())
				Expect(ignore(other)).To(MatchError(other))
				Expect(is(errors.New(code))).To(BeFalse())
			},
			Entry("dry run", awserrors.IsDryRunError, awserrors.IgnoreDryRunError, "DryRunOperation"),
			Entry("unauthorized operation", awserrors.IsUnauthorizedOperationError, awserrors.IgnoreUnauthorizedOperationError, "UnauthorizedOperation"),
			Entry("rate limited", awserrors.IsRateLimitedError, awserrors.IgnoreRateLimitedError, "RequestLimitExceeded"),
		)
		It("should match server errors by fault rather than code", func() {
			serverErr := fmt.Errorf("wrapped, %w", &smithy.GenericAPIError{Code: "InternalError", Fault: smithy.FaultServer})
			Expect(awserrors.IsServerError(serverErr)).To(BeTrue())
			Expect(awserrors.IgnoreServerError(serverErr)).To(Succeed())

			clientErr := &smithy.GenericAPIError{Code: "InternalError", Fault: smithy.FaultClient}
			Expect(awserrors.IsServerError(clientErr)).To(BeFalse())
			Expect(awserrors.IgnoreServerError(clientErr)).To(MatchError(clientErr))
			Expect(awserrors.IsServerError(nil)).To(BeFalse())
			Expect(awserrors.IsServerError(errors.New("InternalError"))).To(BeFalse())
		})
		It("should only match a launch template not found by name", func() {
			Expect(awserrors.IsLaunchTemplateNotFound(apiError("InvalidLaunchTemplateName.NotFoundException", ""))).To(BeTrue())
			Expect(awserrors.IsLaunchTemplateNotFound(apiError("InvalidLaunchTemplateId.NotFound", ""))).To(BeFalse())
			Expect(awserrors.IsLaunchTemplateNotFound(nil)).To(BeFalse())
		})
		It("should require both the code and the message for an instance profile not found", func() {
			Expect(awserrors.IsInstanceProfileNotFound(apiError("InvalidParameterValue", "Value (my-profile) for parameter iamInstanceProfile.name is invalid. Invalid IAM Instance Profile name"))).To(BeTrue())
			Expect(awserrors.IsInstanceProfileNotFound(apiError("InvalidParameterValue", "Invalid value for parameter subnetId"))).To(BeFalse())
			Expect(awserrors.IsInstanceProfileNotFound(apiError("UnauthorizedOperation", "Invalid IAM Instance Profile name"))).To(BeFalse())
			Expect(awserrors.IsInstanceProfileNotFound(nil)).To(BeFalse())
		})
		It("should require both the code and the message for user data that is too large", func() {
			Expect(awserrors.IsUserDataTooLarge(apiError("InvalidUserData.Malformed", "User data is limited to 16384 bytes"))).To(BeTrue())
			Expect(awserrors.IsUserDataTooLarge(apiError("InvalidUserData.Malformed", "User data must be base64 encoded"))).To(BeFalse())
			Expect(awserrors.IsUserDataTooLarge(apiError("InvalidParameterValue", "User data is limited to 16384 bytes"))).To(BeFalse())
			Expect(awserrors.IsUserDataTooLarge(nil)).To(BeFalse())
		})
	})
	Context("CreateFleet error predicates", func() {
		It("should treat temporary capacity errors as unfulfillable", func() {
			for _, code := range []string{
				"InsufficientInstanceCapacity",
				"MaxSpotInstanceCountExceeded",
				"VcpuLimitExceeded",
				"UnfulfillableCapacity",
				"Unsupported",
				"InsufficientFreeAddressesInSubnet",
				"MaxFleetCountExceeded",
				"SpotMaxPriceTooLow",
				"ReservationCapacityExceeded",
				"InvalidCapacityReservationState.Unavailable",
			} {
				Expect(awserrors.IsUnfulfillableCapacity(fleetError(code, ""))).To(BeTrue(), code)
			}
		})
		It("should not treat other fleet errors as unfulfillable", func() {
			for _, code := range []string{
				"AuthFailure.ServiceLinkedRoleCreationNotPermitted",
				"InvalidLaunchTemplateId.NotFound",
				"InternalError",
				"UnauthorizedOperation",
			} {
				Expect(awserrors.IsUnfulfillableCapacity(fleetError(code, ""))).To(BeFalse(), code)
			}
		})
		It("should match single-code fleet errors", func() {
			Expect(awserrors.IsServiceLinkedRoleCreationNotPermitted(fleetError("AuthFailure.ServiceLinkedRoleCreationNotPermitted", ""))).To(BeTrue())
			Expect(awserrors.IsServiceLinkedRoleCreationNotPermitted(fleetError("AuthFailure", ""))).To(BeFalse())
			Expect(awserrors.IsInsufficientFreeAddressesInSubnet(fleetError("InsufficientFreeAddressesInSubnet", ""))).To(BeTrue())
			Expect(awserrors.IsInsufficientFreeAddressesInSubnet(fleetError("InsufficientInstanceCapacity", ""))).To(BeFalse())
			Expect(awserrors.IsReservationCapacityExceeded(fleetError("ReservationCapacityExceeded", ""))).To(BeTrue())
			Expect(awserrors.IsReservationCapacityExceeded(fleetError("InsufficientInstanceCapacity", ""))).To(BeFalse())
		})
		It("should detect the spread placement group limit from the message", func() {
			Expect(awserrors.IsSpreadPlacementGroupLimitError(fleetError("InsufficientInstanceCapacity",
				"You've reached the limit of instances in this spread placement group. A spread placement group can have up to seven instances per Availability Zone."))).To(BeTrue())
			Expect(awserrors.IsSpreadPlacementGroupLimitError(fleetError("InsufficientInstanceCapacity",
				"There is no Spot capacity available that matches your request."))).To(BeFalse())
			Expect(awserrors.IsSpreadPlacementGroupLimitError(ec2types.CreateFleetError{ErrorCode: lo.ToPtr("InsufficientInstanceCapacity")})).To(BeFalse())
		})
	})
	Context("ToReasonMessage", func() {
		DescribeTable("should classify launch errors",
			func(err error, expectedReason, expectedMessage string) {
				reason, message := awserrors.ToReasonMessage(err)
				Expect(reason).To(Equal(expectedReason))
				Expect(message).To(Equal(expectedMessage))
			},
			Entry("spot service linked role, ahead of the generic AuthFailure match",
				errors.New("AuthFailure.ServiceLinkedRoleCreationNotPermitted: The provided credentials do not have permission to create the service-linked role for EC2 Spot Instances."),
				"SpotSLRCreationFailed", "User does not have sufficient permission to create the Spot ServiceLinkedRole to launch spot instances"),
			Entry("permission boundary deny",
				errors.New("UnauthorizedOperation: You are not authorized to perform: ec2:RunInstances with an explicit deny in a permissions boundary"),
				"Unauthorized", "User is not authorized to perform this operation due to a permission boundary"),
			Entry("service control policy deny",
				errors.New("AccessDenied: User is not authorized to perform: ec2:CreateFleet with an explicit deny in a service control policy"),
				"Unauthorized", "User is not authorized to perform this operation due to a service control policy"),
			Entry("AMI authorization",
				errors.New("AuthFailure: Not authorized for images: [ami-0123456789abcdef0]"),
				"AMIAuthorizationFailure", "User is not authorized for AMI used in instance launch"),
			Entry("missing identity-based policy",
				errors.New("UnauthorizedOperation: You are not authorized to perform this operation."),
				"Unauthorized", "User is not authorized to perform this operation because no identity-based policy allows it"),
			Entry("security group and subnet VPC mismatch",
				errors.New("InvalidParameter: Security group sg-123 and subnet subnet-456 belong to different networks."),
				"SecurityGroupSubnetVPCMismatch", "Security groups and subnets must be in the same VPC."),
			Entry("invalid instance profile name",
				errors.New("InvalidParameterValue: Value (my-profile) for parameter iamInstanceProfile.name is invalid. Invalid IAM Instance Profile name"),
				"InstanceProfileNameInvalid", "Instance profile name used from EC2NodeClass status does not exist"),
			Entry("launch template not found",
				errors.New("InvalidLaunchTemplateId.NotFound: The specified launch template, with template ID lt-123, does not exist."),
				"LaunchTemplateNotFound", "Launch template used for instance launch wasn't found"),
			Entry("user data too large",
				errors.New("InvalidUserData.Malformed: User data is limited to 16384 bytes"),
				"UserDataSizeLimitExceeded", "Rendered user data exceeds the EC2 user data size limit"),
			Entry("malformed AMI ID",
				errors.New("InvalidAMIID.Malformed: Invalid id: \"ami-bad\""),
				"InvalidAMIID", "AMI used for instance launch is invalid"),
			Entry("AMI not found",
				errors.New("InvalidAMIID.NotFound: The image id '[ami-123]' does not exist"),
				"AMINotFound", "AMI used for instance launch either does not exist or you don't have permissions to use it"),
			Entry("request limit exceeded",
				errors.New("RequestLimitExceeded: Request limit exceeded."),
				"RequestLimitExceeded", "Request limit exceeded"),
			Entry("internal error",
				errors.New("InternalError: An internal error has occurred"),
				"InternalError", "An internal error has occurred"),
			Entry("free tier ineligible",
				errors.New("InvalidParameterCombination: The specified instance type is not eligible for Free Tier."),
				"FreeTierIneligible", "The specified instance type is not eligible for Free Tier"),
			Entry("fleet quota",
				errors.New("MaxFleetCountExceeded: You've reached your quota for maximum Fleet Requests for this account."),
				"FleetQuotaExceeded", "A fleet launch was requested but this would exceed your fleet request quota"),
			Entry("account pending verification",
				errors.New("PendingVerification: Your request for accessing resources in this region is being validated."),
				"AccountPendingVerification", "An instance launch was requested but the request for launching resources in this region is still being verified"),
			Entry("spot quota",
				errors.New("MaxSpotInstanceCountExceeded: Max spot instance count exceeded"),
				"SpotQuotaExceeded", "A spot instance launch was requested but this would exceed your spot instance quota"),
			Entry("vCPU quota",
				errors.New("VcpuLimitExceeded: You have requested more vCPU capacity than your current vCPU limit allows"),
				"VCPULimitExceeded", "An instance was requested that would exceed your VCPU quota"),
			Entry("subnet out of IP addresses",
				errors.New("InsufficientFreeAddressesInSubnet: There are not enough free addresses in subnet 'subnet-123' to satisfy the requested number of instances."),
				"InsufficientFreeAddressesInSubnet", "There are not enough free IP addresses to launch an instance in this subnet"),
			Entry("unrecognized error",
				errors.New("SomeBrandNewEC2ErrorCode: unexpected"),
				"LaunchFailed", "Instance launch failed"),
		)
		It("should prefer a specific error over a capacity error when fleet errors are combined", func() {
			// combineFleetErrors joins every unique fleet error, so a capacity error can share the string
			// with a more actionable one. The capacity errors are matched last so they don't hide it.
			errs := multierr.Combine(
				errors.New("VcpuLimitExceeded: You have requested more vCPU capacity than your current vCPU limit allows"),
				errors.New("MaxSpotInstanceCountExceeded: Max spot instance count exceeded"),
				errors.New("InvalidLaunchTemplateId.NotFound: The specified launch template does not exist."),
			)
			reason, _ := awserrors.ToReasonMessage(errs)
			Expect(reason).To(Equal("LaunchTemplateNotFound"))

			errs = multierr.Combine(
				errors.New("InsufficientFreeAddressesInSubnet: There are not enough free addresses in subnet 'subnet-123'"),
				errors.New("UnauthorizedOperation: You are not authorized to perform this operation."),
			)
			reason, _ = awserrors.ToReasonMessage(errs)
			Expect(reason).To(Equal("Unauthorized"))
		})
		It("should classify an error by its wrapped API error text", func() {
			err := fmt.Errorf("creating fleet, %w", &smithy.GenericAPIError{Code: "RequestLimitExceeded", Message: "Request limit exceeded."})
			reason, _ := awserrors.ToReasonMessage(err)
			Expect(reason).To(Equal("RequestLimitExceeded"))
		})
	})
})
