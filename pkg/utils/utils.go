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

package utils

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/awslabs/operatorpkg/serrors"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"

	"github.com/samber/lo"
)

var (
	instanceIDRegex = regexp.MustCompile(`(?P<Provider>.*):///(?P<AZ>.*)/(?P<InstanceID>.*)`)
)

// ParseInstanceID parses the provider ID stored on the node to get the instance ID
// associated with a node
func ParseInstanceID(providerID string) (string, error) {
	matches := instanceIDRegex.FindStringSubmatch(providerID)
	if matches == nil {
		return "", serrors.Wrap(fmt.Errorf("provider id does not match known format"), "provider-id", providerID)
	}
	for i, name := range instanceIDRegex.SubexpNames() {
		if name == "InstanceID" {
			return matches[i], nil
		}
	}
	return "", serrors.Wrap(fmt.Errorf("provider id does not match known format"), "provider-id", providerID)
}

// EC2MergeTags takes a variadic list of maps and merges them together into a list of
// EC2 tags to be passed into EC2 API calls
func EC2MergeTags(tags ...map[string]string) []ec2types.Tag {
	return lo.MapToSlice(lo.Assign(tags...), func(k, v string) ec2types.Tag {
		return ec2types.Tag{Key: aws.String(k), Value: aws.String(v)}
	})
}

// EC2MergeTags takes a variadic list of maps and merges them together into a list of
// EC2 tags to be passed into EC2 API calls
func IAMMergeTags(tags ...map[string]string) []iamtypes.Tag {
	return lo.MapToSlice(lo.Assign(tags...), func(k, v string) iamtypes.Tag {
		return iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)}
	})
}

// PrettySlice truncates a slice after a certain number of max items to ensure
// that the Slice isn't too long
func PrettySlice[T any](s []T, maxItems int) string {
	var sb strings.Builder
	for i, elem := range s {
		if i > maxItems-1 {
			fmt.Fprintf(&sb, " and %d other(s)", len(s)-i)
			break
		} else if i > 0 {
			fmt.Fprint(&sb, ", ")
		}
		fmt.Fprint(&sb, elem)
	}
	return sb.String()
}

// WithDefaultFloat64 returns the float64 value of the supplied environment variable or, if not present,
// the supplied default value. If the float64 conversion fails, returns the default
func WithDefaultFloat64(key string, def float64) float64 {
	val, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return def
	}
	return f
}

func GetTags(nodeClass *v1.EC2NodeClass, nodeClaim *karpv1.NodeClaim, clusterName string) (map[string]string, error) {
	var invalidTags []string
	for key := range nodeClass.Spec.Tags {
		for _, exp := range v1.RestrictedTagPatterns {
			if exp.MatchString(key) {
				invalidTags = append(invalidTags, key)
				break
			}
		}
	}
	if len(invalidTags) != 0 {
		quotedTags := lo.Map(invalidTags, func(tag string, _ int) string {
			return fmt.Sprintf("%q", tag)
		})
		return nil, serrors.Wrap(fmt.Errorf("tags failed validation requirements"), "tags", strings.Join(quotedTags, ", "))
	}
	staticTags := map[string]string{
		fmt.Sprintf("kubernetes.io/cluster/%s", clusterName): "owned",
		karpv1.NodePoolLabelKey:                              nodeClaim.Labels[karpv1.NodePoolLabelKey],
		v1.EKSClusterNameTagKey:                              clusterName,
		v1.LabelNodeClass:                                    nodeClass.Name,
	}
	return lo.Assign(nodeClass.Spec.Tags, staticTags), nil
}

// CanonicalFilterSetKey returns a deterministic, collision-free cache key for a
// single resolved EC2 filter set. A filter set is the list of filters passed to
// one Describe* call, which the EC2 API evaluates as a logical AND. Filters and
// the values within each filter are sorted so that semantically identical filter
// sets map to the same key regardless of the order the selector terms were
// authored in, while any difference in filter names, values, or the number of
// filters (i.e. AND grouping) produces a distinct key.
//
// This encoding is used to cache selector resolution per filter set rather than
// per NodeClass, allowing NodeClasses that share a selector term to share the
// underlying cache entry (see #9063). Unlike hashstructure with SlicesAsSets —
// which collapsed nested AND/OR grouping and hashed slices containing duplicate
// elements to zero (see #8619) — this preserves the exact structure of the
// filter set, so distinct selectors can never alias.
func CanonicalFilterSetKey(filters []ec2types.Filter) string {
	// Marshal each filter independently (with its values sorted), then sort the
	// encoded filters. Because each encoded filter is a self-delimiting JSON
	// object, the concatenation is injective over the multiset of filters, and
	// sorting makes it independent of filter ordering within the set.
	encoded := make([]string, 0, len(filters))
	for _, filter := range filters {
		values := append([]string(nil), filter.Values...)
		sort.Strings(values)
		b, _ := json.Marshal(struct {
			Name   string
			Values []string
		}{Name: aws.ToString(filter.Name), Values: values})
		encoded = append(encoded, string(b))
	}
	sort.Strings(encoded)
	return strings.Join(encoded, "")
}
