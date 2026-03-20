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

package resourcetypes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cloudformationtypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"go.uber.org/zap"
)

type Stack struct {
	cloudFormationClient *cloudformation.Client
	iamClient            *iam.Client
	ec2Client            *ec2.Client
	logger               *zap.SugaredLogger
}

func NewStack(cloudFormationClient *cloudformation.Client, iamClient *iam.Client, ec2Client *ec2.Client) *Stack {
	logger := lo.Must(zap.NewProduction()).Sugar()
	return &Stack{
		cloudFormationClient: cloudFormationClient,
		iamClient:            iamClient,
		ec2Client:            ec2Client,
		logger:               logger,
	}
}

func (s *Stack) String() string {
	return "CloudformationStacks"
}

func (s *Stack) Global() bool {
	return false
}

func (s *Stack) GetExpired(ctx context.Context, expirationTime time.Time, excludedClusters []string) (names []string, err error) {
	stacks, err := s.getAllStacks(ctx)
	if err != nil {
		return names, err
	}

	activeStacks := lo.Reject(stacks, func(s cloudformationtypes.Stack, _ int) bool {
		return s.StackStatus == cloudformationtypes.StackStatusDeleteComplete ||
			s.StackStatus == cloudformationtypes.StackStatusDeleteInProgress
	})
	for _, stack := range activeStacks {
		stackName := lo.FromPtr(stack.StackName)

		// Special handling for REVIEW_IN_PROGRESS stacks - these are stuck waiting for approval
		// and often have no tags because they never completed creation
		if stack.StackStatus == cloudformationtypes.StackStatusReviewInProgress {
			if lo.FromPtr(stack.CreationTime).Before(expirationTime) {
				// Check if stack has any actual resources
				hasResources, checkErr := s.stackHasResources(ctx, stackName)
				if checkErr != nil {
					s.logger.Warnf("failed to check resources for REVIEW_IN_PROGRESS stack %s: %v (skipping for safety)",
						stackName, checkErr)
					continue
				}

				if !hasResources {
					s.logger.Infof("REVIEW_IN_PROGRESS stack %s has no resources and is expired, marking for deletion", stackName)
					names = append(names, stackName)
				} else {
					s.logger.Infof("REVIEW_IN_PROGRESS stack %s has resources, skipping for safety", stackName)
				}
			}
			continue
		}

		// Regular tag-based cleanup for non-REVIEW_IN_PROGRESS stacks
		clusterName, found := lo.Find(stack.Tags, func(tag cloudformationtypes.Tag) bool {
			return *tag.Key == karpenterTestingTag
		})
		if found && slices.Contains(excludedClusters, lo.FromPtr(clusterName.Value)) {
			continue
		}
		if _, found := lo.Find(stack.Tags, func(t cloudformationtypes.Tag) bool {
			return lo.FromPtr(t.Key) == karpenterTestingTag || lo.FromPtr(t.Key) == githubRunURLTag
		}); found && lo.FromPtr(stack.CreationTime).Before(expirationTime) {
			names = append(names, stackName)
		}
	}
	return names, err
}

func (s *Stack) CountAll(ctx context.Context) (count int, err error) {
	stacks, err := s.getAllStacks(ctx)
	if err != nil {
		return count, err
	}

	return len(stacks), nil
}

func (s *Stack) Get(ctx context.Context, clusterName string) (names []string, err error) {
	stacks, err := s.getAllStacks(ctx)
	if err != nil {
		return names, err
	}

	for _, stack := range stacks {
		if _, found := lo.Find(stack.Tags, func(t cloudformationtypes.Tag) bool {
			return lo.FromPtr(t.Key) == karpenterTestingTag && lo.FromPtr(t.Value) == clusterName
		}); found {
			names = append(names, lo.FromPtr(stack.StackName))
		}
	}
	return names, nil
}

// Cleanup any old stacks that were provisioned as part of testing
// We execute these in serial since we will most likely get rate limited if we try to delete these too aggressively
func (s *Stack) Cleanup(ctx context.Context, names []string) ([]string, error) {
	var deleted []string
	var errs error
	for i := range names {
		stackLogger := s.logger.With("stack", names[i])
		stackLogger.Infof("processing stack for deletion")

		// Check if stack is in DELETE_FAILED state and needs dependency cleanup
		needsCleanup, err := s.stackNeedsCleanup(ctx, names[i])
		if err != nil {
			stackLogger.Errorf("failed to check stack status: %v", err)
			errs = multierr.Append(errs, fmt.Errorf("failed to check stack status %s: %w", names[i], err))
			continue
		}

		if needsCleanup {
			stackLogger.Warnf("stack is in DELETE_FAILED state, cleaning up dependencies before retry")
			// Clean up dependencies before attempting deletion
			if err := s.cleanupStackDependencies(ctx, names[i]); err != nil {
				stackLogger.Errorf("failed to cleanup dependencies: %v", err)
				errs = multierr.Append(errs, fmt.Errorf("failed to cleanup dependencies for %s: %w", names[i], err))
				continue
			}
			stackLogger.Infof("successfully cleaned up dependencies, retrying stack deletion")
		}

		// Attempt stack deletion
		stackLogger.Infof("initiating stack deletion")
		_, err = s.cloudFormationClient.DeleteStack(ctx, &cloudformation.DeleteStackInput{
			StackName: lo.ToPtr(names[i]),
		})
		if err != nil {
			stackLogger.Errorf("stack deletion failed: %v", err)
			errs = multierr.Append(errs, fmt.Errorf("failed to delete stack %s: %w", names[i], err))
			continue
		}
		stackLogger.Infof("stack deletion initiated successfully")
		deleted = append(deleted, names[i])
	}
	return deleted, errs
}

func (s *Stack) stackNeedsCleanup(ctx context.Context, stackName string) (bool, error) {
	resp, err := s.cloudFormationClient.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
		StackName: lo.ToPtr(stackName),
	})
	if err != nil {
		return false, err
	}
	if len(resp.Stacks) == 0 {
		return false, fmt.Errorf("stack %s not found", stackName)
	}

	// Stack needs cleanup if it's in DELETE_FAILED state
	status := resp.Stacks[0].StackStatus
	return status == cloudformationtypes.StackStatusDeleteFailed, nil
}

func (s *Stack) cleanupStackDependencies(ctx context.Context, stackName string) error {
	stackLogger := s.logger.With("stack", stackName)
	stackLogger.Infof("describing stack resources to find dependencies")

	// Get all resources in the stack
	resp, err := s.cloudFormationClient.DescribeStackResources(ctx, &cloudformation.DescribeStackResourcesInput{
		StackName: lo.ToPtr(stackName),
	})
	if err != nil {
		return fmt.Errorf("failed to describe stack resources: %w", err)
	}

	// First, clean up ENIs from subnets (they block subnet deletion)
	subnetCount := 0
	for _, resource := range resp.StackResources {
		if resource.ResourceType != nil && *resource.ResourceType == "AWS::EC2::Subnet" {
			if resource.PhysicalResourceId == nil {
				continue
			}
			subnetID := *resource.PhysicalResourceId
			subnetCount++

			stackLogger.Infof("found subnet: %s, checking for attached ENIs", subnetID)

			// Delete ENIs attached to this subnet
			if err := s.deleteENIsInSubnet(ctx, subnetID); err != nil {
				stackLogger.Warnf("failed to cleanup ENIs in subnet %s: %v (continuing anyway)", subnetID, err)
				// Don't return error, continue with other cleanup
			}
		}
	}

	if subnetCount > 0 {
		stackLogger.Infof("processed %d subnets for ENI cleanup", subnetCount)
	}

	// Find IAM managed policies that need cleanup
	policyCount := 0
	for _, resource := range resp.StackResources {
		if resource.ResourceType != nil && *resource.ResourceType == "AWS::IAM::ManagedPolicy" {
			if resource.PhysicalResourceId == nil {
				continue
			}
			policyArn := *resource.PhysicalResourceId
			policyCount++

			stackLogger.Infof("found IAM managed policy: %s", policyArn)

			// Detach policy from all attached entities
			if err := s.detachPolicyFromAllEntities(ctx, policyArn); err != nil {
				return fmt.Errorf("failed to detach policy %s: %w", policyArn, err)
			}
		}
	}

	if policyCount == 0 {
		stackLogger.Infof("no IAM managed policies found in stack")
	} else {
		stackLogger.Infof("successfully processed %d IAM policies", policyCount)
	}

	return nil
}

func (s *Stack) deleteENIsInSubnet(ctx context.Context, subnetID string) error {
	subnetLogger := s.logger.With("subnet", subnetID)

	// Find all ENIs in this subnet
	resp, err := s.ec2Client.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{
			{
				Name:   lo.ToPtr("subnet-id"),
				Values: []string{subnetID},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to describe network interfaces: %w", err)
	}

	if len(resp.NetworkInterfaces) == 0 {
		subnetLogger.Infof("no ENIs found in subnet")
		return nil
	}

	subnetLogger.Infof("found %d ENIs in subnet", len(resp.NetworkInterfaces))

	// Delete each ENI
	deletedCount := 0
	for _, eni := range resp.NetworkInterfaces {
		eniID := lo.FromPtr(eni.NetworkInterfaceId)
		eniLogger := s.logger.With("eni", eniID, "subnet", subnetID)

		// Check if ENI is attached to an instance
		if eni.Attachment != nil && eni.Attachment.InstanceId != nil {
			instanceID := *eni.Attachment.InstanceId
			eniLogger.Infof("ENI is attached to instance %s, detaching first", instanceID)

			// Detach ENI from instance
			_, err := s.ec2Client.DetachNetworkInterface(ctx, &ec2.DetachNetworkInterfaceInput{
				AttachmentId: eni.Attachment.AttachmentId,
				Force:        lo.ToPtr(true),
			})
			if err != nil {
				eniLogger.Warnf("failed to detach ENI from instance: %v (will try to delete anyway)", err)
			} else {
				eniLogger.Infof("successfully detached ENI from instance %s", instanceID)
			}
		}

		// Delete the ENI
		eniLogger.Infof("deleting ENI")
		_, err := s.ec2Client.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: lo.ToPtr(eniID),
		})
		if err != nil {
			eniLogger.Warnf("failed to delete ENI: %v", err)
			continue
		}
		eniLogger.Infof("successfully deleted ENI")
		deletedCount++
	}

	subnetLogger.Infof("deleted %d/%d ENIs from subnet", deletedCount, len(resp.NetworkInterfaces))
	return nil
}

func (s *Stack) detachPolicyFromAllEntities(ctx context.Context, policyArn string) error {
	policyLogger := s.logger.With("policy", policyArn)
	policyLogger.Infof("listing entities attached to policy")

	// List all entities attached to this policy
	resp, err := s.iamClient.ListEntitiesForPolicy(ctx, &iam.ListEntitiesForPolicyInput{
		PolicyArn: lo.ToPtr(policyArn),
	})
	if err != nil {
		// If policy doesn't exist, that's fine - it's already cleaned up
		var nse *iamtypes.NoSuchEntityException
		if errors.As(err, &nse) {
			policyLogger.Infof("policy does not exist (already cleaned up)")
			return nil
		}
		return fmt.Errorf("failed to list entities for policy: %w", err)
	}

	totalEntities := len(resp.PolicyRoles) + len(resp.PolicyUsers) + len(resp.PolicyGroups)
	if totalEntities == 0 {
		policyLogger.Infof("no entities attached to policy")
		return nil
	}

	policyLogger.Infof("found %d attached entities (%d roles, %d users, %d groups)",
		totalEntities, len(resp.PolicyRoles), len(resp.PolicyUsers), len(resp.PolicyGroups))

	// Detach from all roles
	for _, role := range resp.PolicyRoles {
		policyLogger.Infof("detaching policy from role: %s", *role.RoleName)
		_, err := s.iamClient.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
			RoleName:  role.RoleName,
			PolicyArn: lo.ToPtr(policyArn),
		})
		if err != nil {
			return fmt.Errorf("failed to detach policy from role %s: %w", *role.RoleName, err)
		}
		policyLogger.Infof("successfully detached policy from role: %s", *role.RoleName)
	}

	// Detach from all users
	for _, user := range resp.PolicyUsers {
		policyLogger.Infof("detaching policy from user: %s", *user.UserName)
		_, err := s.iamClient.DetachUserPolicy(ctx, &iam.DetachUserPolicyInput{
			UserName:  user.UserName,
			PolicyArn: lo.ToPtr(policyArn),
		})
		if err != nil {
			return fmt.Errorf("failed to detach policy from user %s: %w", *user.UserName, err)
		}
		policyLogger.Infof("successfully detached policy from user: %s", *user.UserName)
	}

	// Detach from all groups
	for _, group := range resp.PolicyGroups {
		policyLogger.Infof("detaching policy from group: %s", *group.GroupName)
		_, err := s.iamClient.DetachGroupPolicy(ctx, &iam.DetachGroupPolicyInput{
			GroupName: group.GroupName,
			PolicyArn: lo.ToPtr(policyArn),
		})
		if err != nil {
			return fmt.Errorf("failed to detach policy from group %s: %w", *group.GroupName, err)
		}
		policyLogger.Infof("successfully detached policy from group: %s", *group.GroupName)
	}

	policyLogger.Infof("successfully detached policy from all %d entities", totalEntities)
	return nil
}

func (s *Stack) stackHasResources(ctx context.Context, stackName string) (bool, error) {
	resp, err := s.cloudFormationClient.DescribeStackResources(ctx, &cloudformation.DescribeStackResourcesInput{
		StackName: lo.ToPtr(stackName),
	})
	if err != nil {
		// If we can't describe resources, assume it has resources to be safe
		return true, err
	}

	return len(resp.StackResources) > 0, nil
}

func (s *Stack) stackNeedsChangeSetCleanup(ctx context.Context, stackName string) (bool, error) {
	resp, err := s.cloudFormationClient.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
		StackName: lo.ToPtr(stackName),
	})
	if err != nil {
		return false, err
	}
	if len(resp.Stacks) == 0 {
		return false, fmt.Errorf("stack %s not found", stackName)
	}

	status := resp.Stacks[0].StackStatus
	return status == cloudformationtypes.StackStatusReviewInProgress, nil
}

func (s *Stack) deleteChangeSets(ctx context.Context, stackName string) error {
	stackLogger := s.logger.With("stack", stackName)

	// List all change sets
	resp, err := s.cloudFormationClient.ListChangeSets(ctx, &cloudformation.ListChangeSetsInput{
		StackName: lo.ToPtr(stackName),
	})
	if err != nil {
		return fmt.Errorf("failed to list change sets: %w", err)
	}

	if len(resp.Summaries) == 0 {
		stackLogger.Infof("no change sets found for stack")
		return nil
	}

	stackLogger.Infof("found %d change sets to delete", len(resp.Summaries))

	// Delete each change set
	for _, cs := range resp.Summaries {
		changeSetName := lo.FromPtr(cs.ChangeSetName)
		stackLogger.Infof("deleting change set: %s", changeSetName)
		_, err := s.cloudFormationClient.DeleteChangeSet(ctx, &cloudformation.DeleteChangeSetInput{
			StackName:     lo.ToPtr(stackName),
			ChangeSetName: cs.ChangeSetName,
		})
		if err != nil {
			return fmt.Errorf("failed to delete change set %s: %w", changeSetName, err)
		}
		stackLogger.Infof("successfully deleted change set: %s", changeSetName)
	}

	return nil
}

func (s *Stack) getAllStacks(ctx context.Context) (stacks []cloudformationtypes.Stack, err error) {
	paginator := cloudformation.NewDescribeStacksPaginator(s.cloudFormationClient, &cloudformation.DescribeStacksInput{})

	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			return stacks, err
		}
		stacks = append(stacks, out.Stacks...)
	}

	return stacks, nil
}
