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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sdk "github.com/aws/karpenter-provider-aws/pkg/aws"
	"github.com/aws/karpenter-provider-aws/pkg/batcher"
	awserrors "github.com/aws/karpenter-provider-aws/pkg/errors"
)

const (
	// cacheRetention is how long the tasks of an instance stay cached after the instance was last read or written.
	// Live NodeClaims keep their instance's entry warm; entries for instances that are gone age out.
	cacheRetention = 24 * time.Hour
	gcInterval     = time.Minute
	// metricsMaxAge skips metrics for tasks that completed longer than this before the provider first saw them, which
	// is what cache hydration after a restart or leader change looks like.
	metricsMaxAge = time.Hour

	incorrectInstanceStateErrorCode = "IncorrectInstanceState"
)

// dryRunToken is the TokenTagKey value a dry run requests, so its tags have the same shape as a real create's.
var dryRunToken = Token("dry-run")

// Token returns the client token and TokenTagKey value for an operation. EC2 caps client tokens at 64 characters and
// operation IDs are longer, so the token is the hex SHA-256 of the operation ID.
func Token(operationID string) string {
	sum := sha256.Sum256([]byte(operationID))
	return hex.EncodeToString(sum[:])
}

// TaskConflictError is returned by Create when EC2 rejects the create with IncorrectInstanceState and the instance has
// no task carrying the operation's token. ActiveTaskID is the unfinished task blocking the create, or empty when EC2
// rejected the create for another instance state (for example a stopped instance). It is retryable: once the active
// task finishes, Create can succeed.
type TaskConflictError struct {
	InstanceID   string
	ActiveTaskID string
	err          error
}

func (e *TaskConflictError) Error() string {
	if e.ActiveTaskID != "" {
		return fmt.Sprintf("instance %s has an active replace root volume task %s, %s", e.InstanceID, e.ActiveTaskID, e.err)
	}
	return fmt.Sprintf("instance %s is not in a state that allows replacing its root volume, %s", e.InstanceID, e.err)
}

func (e *TaskConflictError) Unwrap() error { return e.err }

func IsTaskConflictError(err error) bool {
	_, ok := lo.ErrorsAs[*TaskConflictError](err)
	return ok
}

type entry struct {
	task Task
	// observed is set once the task's terminal metrics have been recorded, or deliberately skipped
	observed bool
}

type instanceEntry struct {
	taskIDs    sets.Set[string]
	lastAccess time.Time
}

var _ Provider = (*DefaultProvider)(nil)

type DefaultProvider struct {
	ec2api      sdk.EC2API
	batcher     *batcher.DescribeReplaceRootVolumeTasksBatcher
	clk         clock.Clock
	clusterName string

	mu sync.Mutex
	// tasks holds Karpenter's tasks by task ID. Terminal tasks are served from here; non-terminal ones are always
	// re-read. Tasks Karpenter did not create are never stored.
	tasks map[string]*entry
	// instances indexes tasks by instance ID
	instances map[string]*instanceEntry
	lastGC    time.Time
}

func NewDefaultProvider(ctx context.Context, ec2api sdk.EC2API, clk clock.Clock, clusterName string) *DefaultProvider {
	return &DefaultProvider{
		ec2api:      ec2api,
		batcher:     batcher.NewDescribeReplaceRootVolumeTasksBatcher(ctx, ec2api),
		clk:         clk,
		clusterName: clusterName,
		tasks:       map[string]*entry{},
		instances:   map[string]*instanceEntry{},
		lastGC:      clk.Now(),
	}
}

func (p *DefaultProvider) Create(ctx context.Context, instanceID, imageID, token string) (*Task, error) {
	if token == "" {
		return nil, fmt.Errorf("creating replace root volume task, token is empty")
	}
	// Adopt first. The task may have been created by an earlier attempt whose result was never persisted, and it may
	// already have finished, which a client token alone would not reveal.
	tasks, err := p.describe(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("creating replace root volume task, %w", err)
	}
	if task, ok := p.adopt(instanceID, tasks, token); ok {
		return task, nil
	}
	out, err := p.ec2api.CreateReplaceRootVolumeTask(ctx, p.createInput(instanceID, imageID, token))
	if err != nil {
		if !isIncorrectInstanceState(err) {
			return nil, fmt.Errorf("creating replace root volume task, %w", err)
		}
		// A task is already active on the instance. If it is ours (a concurrent or retried create), adopt it.
		tasks, derr := p.describe(ctx, instanceID)
		if derr != nil {
			return nil, fmt.Errorf("creating replace root volume task, %w", errors.Join(err, derr))
		}
		if task, ok := p.adopt(instanceID, tasks, token); ok {
			return task, nil
		}
		active, _ := lo.Find(tasks, func(t *Task) bool { return !t.Terminal() })
		return nil, &TaskConflictError{InstanceID: instanceID, ActiveTaskID: lo.TernaryF(active != nil, func() string { return active.ID }, func() string { return "" }), err: err}
	}
	if out.ReplaceRootVolumeTask == nil {
		return nil, fmt.Errorf("creating replace root volume task, empty response")
	}
	task := newTask(*out.ReplaceRootVolumeTask)
	// The create response is ours by construction even if EC2 omits the tags or IDs from it
	task.Token = token
	task.InstanceID = lo.CoalesceOrEmpty(task.InstanceID, instanceID)
	task.ImageID = lo.CoalesceOrEmpty(task.ImageID, imageID)
	if task.ID == "" {
		return nil, fmt.Errorf("creating replace root volume task, response has no task ID")
	}
	log.FromContext(ctx).WithValues("instance-id", instanceID, "image-id", imageID, "task-id", task.ID).V(1).Info("created replace root volume task")
	p.store(task)
	return lo.ToPtr(task), nil
}

// DryRun asks EC2 whether the caller may replace the instance's root volume from imageID, with the same parameters
// Create uses. It returns nil when EC2 answers DryRunOperation, and EC2's error otherwise, such as
// UnauthorizedOperation when the role lacks a permission the real call needs.
func (p *DefaultProvider) DryRun(ctx context.Context, instanceID, imageID string) error {
	input := p.createInput(instanceID, imageID, dryRunToken)
	// A dry run never creates a task, so it carries no client token that a later create could collide with.
	input.ClientToken = nil
	input.DryRun = aws.Bool(true)
	_, err := p.ec2api.CreateReplaceRootVolumeTask(ctx, input)
	switch {
	case awserrors.IsDryRunError(err):
		return nil
	case err != nil:
		return fmt.Errorf("dry run of replace root volume task, %w", err)
	default:
		return fmt.Errorf("dry run of replace root volume task, EC2 didn't answer DryRunOperation")
	}
}

// createInput is the CreateReplaceRootVolumeTask request for a replacement, shared by Create and DryRun so the dry
// run asks for exactly the permissions the real call needs.
func (p *DefaultProvider) createInput(instanceID, imageID, token string) *ec2.CreateReplaceRootVolumeTaskInput {
	return &ec2.CreateReplaceRootVolumeTaskInput{
		InstanceId: aws.String(instanceID),
		ImageId:    aws.String(imageID),
		// EC2 keeps the old root volume as an available, billed volume unless asked to delete it.
		DeleteReplacedRootVolume: aws.Bool(true),
		ClientToken:              aws.String(token),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeReplaceRootVolumeTask,
			Tags: []ec2types.Tag{
				{Key: aws.String(TokenTagKey), Value: aws.String(token)},
				{Key: aws.String(fmt.Sprintf("kubernetes.io/cluster/%s", p.clusterName)), Value: aws.String("owned")},
			},
		}},
	}
}

func (p *DefaultProvider) Latest(ctx context.Context, instanceID string) (*Task, error) {
	if task := p.cachedLatest(instanceID); task != nil && task.Terminal() {
		return task, nil
	}
	if _, err := p.describe(ctx, instanceID); err != nil {
		return nil, fmt.Errorf("getting latest replace root volume task, %w", err)
	}
	// Reading back from the cache keeps a just-created task visible if EC2 has not listed it yet
	return p.cachedLatest(instanceID), nil
}

func (p *DefaultProvider) Failures(imageID string, since time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return lo.CountBy(lo.Values(p.tasks), func(e *entry) bool {
		return e.task.Ours() && e.task.Failed() && e.task.ImageID == imageID && completed(e.task).After(since)
	})
}

// describe returns every task on the instance, ours or not, and stores ours.
func (p *DefaultProvider) describe(ctx context.Context, instanceID string) ([]*Task, error) {
	out, err := p.batcher.DescribeReplaceRootVolumeTasks(ctx, &ec2.DescribeReplaceRootVolumeTasksInput{
		Filters: []ec2types.Filter{{Name: aws.String("instance-id"), Values: []string{instanceID}}},
	})
	if err != nil {
		return nil, err
	}
	tasks := lo.Map(out.ReplaceRootVolumeTasks, func(t ec2types.ReplaceRootVolumeTask, _ int) *Task { return lo.ToPtr(newTask(t)) })
	for _, task := range tasks {
		if task.Ours() && task.ID != "" {
			p.store(*task)
		}
	}
	return tasks, nil
}

func (p *DefaultProvider) store(task Task) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gc()
	e, ok := p.tasks[task.ID]
	if !ok {
		e = &entry{}
		p.tasks[task.ID] = e
	} else if e.task.Terminal() && !task.Terminal() {
		// Terminal tasks never change, so a non-terminal read is stale
		return
	}
	seenRunning := ok && !e.task.Terminal()
	e.task = task
	if task.Terminal() && !e.observed {
		e.observed = true
		if seenRunning || p.clk.Since(completed(task)) <= metricsMaxAge {
			observe(task)
		}
	}
	inst, ok := p.instances[task.InstanceID]
	if !ok {
		inst = &instanceEntry{taskIDs: sets.New[string]()}
		p.instances[task.InstanceID] = inst
	}
	inst.taskIDs.Insert(task.ID)
	inst.lastAccess = p.clk.Now()
}

// cachedLatest returns a copy of the instance's cached task with the latest start time.
func (p *DefaultProvider) cachedLatest(instanceID string) *Task {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gc()
	inst, ok := p.instances[instanceID]
	if !ok {
		return nil
	}
	inst.lastAccess = p.clk.Now()
	tasks := lo.FilterMap(inst.taskIDs.UnsortedList(), func(id string, _ int) (Task, bool) {
		e, ok := p.tasks[id]
		return lo.FromPtr(e).task, ok
	})
	if len(tasks) == 0 {
		return nil
	}
	sort.Slice(tasks, func(i, j int) bool {
		if !tasks[i].StartTime.Equal(tasks[j].StartTime) {
			return tasks[i].StartTime.After(tasks[j].StartTime)
		}
		return tasks[i].ID > tasks[j].ID
	})
	return lo.ToPtr(tasks[0])
}

// gc drops instances that have not been read or written within cacheRetention. It must be called with p.mu held.
func (p *DefaultProvider) gc() {
	if p.clk.Since(p.lastGC) < gcInterval {
		return
	}
	p.lastGC = p.clk.Now()
	for id, inst := range p.instances {
		if p.clk.Since(inst.lastAccess) < cacheRetention {
			continue
		}
		for taskID := range inst.taskIDs {
			delete(p.tasks, taskID)
		}
		delete(p.instances, id)
	}
}

func observe(task Task) {
	outcome := OutcomeFailed
	switch task.State {
	case ec2types.ReplaceRootVolumeTaskStateSucceeded:
		outcome = OutcomeSucceeded
	case ec2types.ReplaceRootVolumeTaskStateFailedDetached:
		outcome = OutcomeFailedDetached
	}
	labels := map[string]string{outcomeLabel: outcome}
	TasksTotal.Inc(labels)
	if !task.StartTime.IsZero() && !task.CompleteTime.IsZero() && !task.CompleteTime.Before(task.StartTime) {
		TaskDurationSeconds.Observe(task.CompleteTime.Sub(task.StartTime).Seconds(), labels)
	}
}

// completed returns when the task completed, falling back to its start time if EC2 did not report a completion time.
func completed(task Task) time.Time {
	return lo.Ternary(task.CompleteTime.IsZero(), task.StartTime, task.CompleteTime)
}

// adopt returns the task carrying token from a describe result, or from the cache if EC2 no longer lists it.
func (p *DefaultProvider) adopt(instanceID string, described []*Task, token string) (*Task, bool) {
	if task, ok := lo.Find(described, func(t *Task) bool { return t.Token == token }); ok {
		return task, true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	inst, ok := p.instances[instanceID]
	if !ok {
		return nil, false
	}
	for id := range inst.taskIDs {
		if e, ok := p.tasks[id]; ok && e.task.Token == token {
			return lo.ToPtr(e.task), true
		}
	}
	return nil, false
}

func newTask(t ec2types.ReplaceRootVolumeTask) Task {
	return Task{
		ID:           aws.ToString(t.ReplaceRootVolumeTaskId),
		InstanceID:   aws.ToString(t.InstanceId),
		ImageID:      aws.ToString(t.ImageId),
		State:        t.TaskState,
		StartTime:    parseTime(t.StartTime),
		CompleteTime: parseTime(t.CompleteTime),
		Token: lo.FromPtr(lo.FindOrElse(t.Tags, ec2types.Tag{}, func(tag ec2types.Tag) bool {
			return aws.ToString(tag.Key) == TokenTagKey
		}).Value),
	}
}

// timeLayouts are the formats EC2 uses for task times, which the SDK models as strings. The API reference shows
// ISO 8601 and the CLI documentation shows "2006-01-02 15:04:05".
var timeLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05"}

// parseTime returns the zero time for an empty or unrecognized value.
func parseTime(s *string) time.Time {
	if aws.ToString(s) == "" {
		return time.Time{}
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, *s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func isIncorrectInstanceState(err error) bool {
	apiErr, ok := lo.ErrorsAs[smithy.APIError](err)
	return ok && apiErr.ErrorCode() == incorrectInstanceStateErrorCode
}
