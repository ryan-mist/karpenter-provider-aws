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

package fake

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/samber/lo"
)

// SnapshotAPI fakes ec2:DescribeSnapshots. Snapshots maps each snapshot the account can create a volume from to the
// restorable-by values it matches: "self" for snapshots the account owns or that are shared with it, "all" for public
// ones. Snapshots missing from the map aren't visible to the account. Like EC2, a call without restorable-by values
// returns every visible snapshot, and listing an invisible one in SnapshotIds fails the whole call.
type SnapshotAPI struct {
	mu        sync.Mutex
	Snapshots map[string][]string
	Calls     []*ec2.DescribeSnapshotsInput
	// Error, when set, is returned by every call.
	Error error
}

func NewSnapshotAPI() *SnapshotAPI {
	return &SnapshotAPI{Snapshots: map[string][]string{}}
}

func (s *SnapshotAPI) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Snapshots = map[string][]string{}
	s.Calls = nil
	s.Error = nil
}

// SetReadable makes the snapshot restorable by the caller's account.
func (s *SnapshotAPI) SetReadable(snapshotID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Snapshots[snapshotID] = []string{"self"}
}

// SetPublic makes the snapshot public, which lets every account create a volume from it.
func (s *SnapshotAPI) SetPublic(snapshotID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Snapshots[snapshotID] = []string{"all"}
}

func (s *SnapshotAPI) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Calls)
}

func (s *SnapshotAPI) DescribeSnapshots(_ context.Context, input *ec2.DescribeSnapshotsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, input)
	if s.Error != nil {
		return nil, s.Error
	}
	for _, id := range input.SnapshotIds {
		if _, ok := s.Snapshots[id]; !ok {
			return nil, &smithy.GenericAPIError{Code: "InvalidSnapshot.NotFound", Message: fmt.Sprintf("The snapshot '%s' does not exist.", id)}
		}
	}
	var ids []string
	for _, f := range input.Filters {
		if aws.ToString(f.Name) == "snapshot-id" {
			ids = append(ids, f.Values...)
		}
	}
	ids = append(ids, input.SnapshotIds...)
	out := &ec2.DescribeSnapshotsOutput{}
	for _, id := range lo.Uniq(ids) {
		restorableBy, ok := s.Snapshots[id]
		if !ok {
			continue
		}
		if len(input.RestorableByUserIds) > 0 && !lo.SomeBy(input.RestorableByUserIds, func(u string) bool { return slices.Contains(restorableBy, u) }) {
			continue
		}
		out.Snapshots = append(out.Snapshots, ec2types.Snapshot{SnapshotId: aws.String(id)})
	}
	return out, nil
}
