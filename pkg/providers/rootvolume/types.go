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

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// TokenTagKey tags every replace root volume task Karpenter creates with the SHA-256 of its operationID. EC2 does
// not return a task's client token, so the tag is how Karpenter recognizes its own tasks.
const TokenTagKey = "karpenter.k8s.aws/replace-root-volume-token" //nolint:gosec // a tag key, not a credential

// Task is a replace root volume task on an instance.
type Task struct {
	ID         string
	InstanceID string
	// ImageID is the AMI the task replaces the root volume from. It is empty for launch-state and snapshot tasks.
	ImageID      string
	State        ec2types.ReplaceRootVolumeTaskState
	StartTime    time.Time
	CompleteTime time.Time
	// Token is the value of TokenTagKey, or empty for tasks Karpenter did not create.
	Token string
}

// Ours returns whether Karpenter created the task.
func (t *Task) Ours() bool { return t != nil && t.Token != "" }

// Terminal returns whether the task has finished. Unknown states are not terminal.
func (t *Task) Terminal() bool {
	switch t.State {
	case ec2types.ReplaceRootVolumeTaskStateSucceeded, ec2types.ReplaceRootVolumeTaskStateFailed,
		ec2types.ReplaceRootVolumeTaskState("failed-detached"):
		return true
	default:
		return false
	}
}

// Succeeded returns whether the task replaced the root volume.
func (t *Task) Succeeded() bool { return t.State == ec2types.ReplaceRootVolumeTaskStateSucceeded }

// Failed returns whether the task finished without replacing the root volume.
func (t *Task) Failed() bool { return t.Terminal() && !t.Succeeded() }

// Provider makes every CreateReplaceRootVolumeTask and DescribeReplaceRootVolumeTasks call and caches terminal tasks.
type Provider interface {
	// Create replaces the instance's root volume from imageID for the operation identified by token. If a task tagged
	// with token already exists on the instance, in any state, it is returned instead of creating another.
	Create(ctx context.Context, instanceID, imageID, token string) (*Task, error)
	// DryRun checks with a DryRun CreateReplaceRootVolumeTask, using the same parameters as Create, whether the caller
	// may replace the instance's root volume from imageID. It returns nil when the call would be allowed.
	DryRun(ctx context.Context, instanceID, imageID string) error
	// Latest returns the most recent task Karpenter created on the instance, or nil if there is none. It reads the
	// cache and describes the instance on a miss.
	Latest(ctx context.Context, instanceID string) (*Task, error)
	// RecordRejection remembers that Karpenter gave up replacing the instance's root volume from imageID, because a
	// check failed or EC2 deterministically rejected the task when the reboot was issued. reason names the check or
	// the EC2 error code. Records are kept in memory only, so they are lost on restart.
	RecordRejection(instanceID, imageID, reason string)
	// Rejection returns the reason recorded for the instance and imageID, if any.
	Rejection(instanceID, imageID string) (string, bool)
	// LastRejection returns when a rejection toward imageID was last recorded on any instance, or the zero time.
	LastRejection(imageID string) time.Time
	// Failures returns Karpenter's failed tasks toward imageID that completed after since, across all instances. It
	// only reads the cache.
	Failures(imageID string, since time.Time) int
}
