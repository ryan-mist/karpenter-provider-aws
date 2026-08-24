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

package amifamily

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"k8s.io/utils/clock"

	"github.com/aws/karpenter-provider-aws/pkg/errors"
	"github.com/aws/karpenter-provider-aws/pkg/utils"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	sdk "github.com/aws/karpenter-provider-aws/pkg/aws"
	"github.com/aws/karpenter-provider-aws/pkg/providers/version"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/aws/karpenter-provider-aws/pkg/providers/ssm"
)

type Provider interface {
	List(ctx context.Context, nodeClass *v1.EC2NodeClass) (AMIs, error)
	ListUncached(ctx context.Context, nodeClass *v1.EC2NodeClass) (AMIs, error)
}

type DefaultProvider struct {
	sync.Mutex

	clk             clock.Clock
	cache           *cache.Cache
	ec2api          sdk.EC2API
	versionProvider version.Provider
	ssmProvider     ssm.Provider
}

func NewDefaultProvider(clk clock.Clock, versionProvider version.Provider, ssmProvider ssm.Provider, ec2api sdk.EC2API, cache *cache.Cache) *DefaultProvider {
	return &DefaultProvider{
		clk:             clk,
		cache:           cache,
		ec2api:          ec2api,
		versionProvider: versionProvider,
		ssmProvider:     ssmProvider,
	}
}

// List returns a list of AMIs with their associated requirements, using the AMI
// cache to avoid redundant DescribeImages calls.
func (p *DefaultProvider) List(ctx context.Context, nodeClass *v1.EC2NodeClass) (AMIs, error) {
	return p.list(ctx, nodeClass, true)
}

// ListUncached is identical to List but bypasses the AMI cache read, forcing a
// fresh DescribeImages call. It is used by the SSM invalidation controller,
// which must observe up-to-date AMI deprecation status rather than a possibly
// stale cached result. Freshly resolved AMIs are still written back to the cache
// for subsequent List callers.
func (p *DefaultProvider) ListUncached(ctx context.Context, nodeClass *v1.EC2NodeClass) (AMIs, error) {
	return p.list(ctx, nodeClass, false)
}

func (p *DefaultProvider) list(ctx context.Context, nodeClass *v1.EC2NodeClass, useCache bool) (AMIs, error) {
	p.Lock()
	defer p.Unlock()
	amis, err := p.amis(ctx, nodeClass, useCache)
	if err != nil {
		return nil, err
	}
	amis.Sort()
	return amis, nil
}

//nolint:gocyclo
func (p *DefaultProvider) DescribeImageQueries(ctx context.Context, nodeClass *v1.EC2NodeClass) ([]DescribeImageQuery, error) {
	// Aliases are mutually exclusive, both on the term level and field level within a term.
	// This is enforced by a CEL validation, we will treat this as an invariant.
	if alias := nodeClass.Alias(); alias != nil {
		kubernetesVersion := p.versionProvider.Get(ctx)
		if alias.Family == v1.AMIFamilyAL2 {
			minorVersion, err := strconv.Atoi(strings.Split(kubernetesVersion, ".")[1])
			if err == nil && minorVersion >= 33 {
				return nil, &AL2DeprecationError{
					error: fmt.Errorf("AL2 aliases are no longer supported on EKS 1.33+ (see https://docs.aws.amazon.com/eks/latest/userguide/kubernetes-versions-standard.html#kubernetes-1-33)"),
				}
			}
		}
		if alias.Family == v1.AMIFamilyWindows2025 {
			minorVersion, err := strconv.Atoi(strings.Split(kubernetesVersion, ".")[1])
			if err == nil && minorVersion < 35 {
				return nil, &WS2025UnsupportedVersionError{
					error: fmt.Errorf("Windows Server 2025 requires EKS version 1.35 or higher, current version: %s", kubernetesVersion),
				}
			}
		}
		query, err := GetAMIFamily(alias.Family, nil).DescribeImageQuery(ctx, p.ssmProvider, kubernetesVersion, alias.Version)
		if err != nil {
			return []DescribeImageQuery{}, err
		}
		return []DescribeImageQuery{query}, nil
	}

	idFilter := ec2types.Filter{Name: aws.String("image-id")}
	queries := []DescribeImageQuery{}
	for _, term := range nodeClass.Spec.AMISelectorTerms {
		switch {
		case term.ID != "":
			idFilter.Values = append(idFilter.Values, term.ID)
		case term.SSMParameter != "":
			imageID, err := p.ssmProvider.Get(ctx, ssm.Parameter{
				Name: term.SSMParameter,
				Type: ssm.CustomParameterType,
			})
			if err != nil {
				if !errors.IsNotFound(err) {
					return []DescribeImageQuery{}, fmt.Errorf("resolving ssm parameter, %w", err)
				}
				log.FromContext(ctx).WithValues("ssmParameter", term.SSMParameter).V(1).Error(err, "parameter not found")
				continue
			}
			if !strings.HasPrefix(imageID, "ami-") {
				log.FromContext(ctx).WithValues("ssmParameter", term.SSMParameter, "id", imageID).V(1).Error(nil, "parameter value is an invalid AMI ID")
				continue
			}
			idFilter.Values = append(idFilter.Values, imageID)
		default:
			query := DescribeImageQuery{
				Owners: lo.Ternary(term.Owner != "", []string{term.Owner}, []string{}),
			}
			if term.Name != "" {
				// Default owners to self,amazon to ensure Karpenter only discovers cross-account AMIs if the user specifically allows it.
				// Removing this default would cause Karpenter to discover publicly shared AMIs passing the name filter.
				query = DescribeImageQuery{
					Owners: lo.Ternary(term.Owner != "", []string{term.Owner}, []string{"self", "amazon"}),
				}
				query.Filters = append(query.Filters, ec2types.Filter{
					Name:   aws.String("name"),
					Values: []string{term.Name},
				})

			}
			for k, v := range term.Tags {
				if v == "*" {
					query.Filters = append(query.Filters, ec2types.Filter{
						Name:   aws.String("tag-key"),
						Values: []string{k},
					})
				} else {
					query.Filters = append(query.Filters, ec2types.Filter{
						Name:   aws.String(fmt.Sprintf("tag:%s", k)),
						Values: []string{v},
					})
				}
			}
			queries = append(queries, query)
		}
	}
	if len(idFilter.Values) > 0 {
		queries = append(queries, DescribeImageQuery{Filters: []ec2types.Filter{idFilter}})
	}
	return queries, nil
}

//nolint:gocyclo
func (p *DefaultProvider) amis(ctx context.Context, nodeClass *v1.EC2NodeClass, useCache bool) (AMIs, error) {
	queries, err := p.DescribeImageQueries(ctx, nodeClass)
	if err != nil {
		return nil, fmt.Errorf("getting AMI queries, %w", err)
	}
	// Resolve each query independently and cache its result keyed on the query's
	// owners and filters (a single DescribeImages call) rather than on the
	// NodeClass identity. NodeClasses that resolve to the same query then share
	// the cache entry, avoiding redundant DescribeImages calls (see #9063), while
	// distinct queries — including ones that differ only by owners — never alias
	// (see #8619).
	images := map[uint64]AMI{}
	for _, query := range queries {
		key := imageQueryCacheKey(query)
		if useCache {
			if cached, ok := p.cache.Get(key); ok {
				mergeAMIs(images, cached.(AMIs))
				continue
			}
		}
		queryImages := map[uint64]AMI{}
		paginator := ec2.NewDescribeImagesPaginator(p.ec2api, query.DescribeImagesInput())
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("describing images, %w", err)
			}
			for _, image := range page.Images {
				arch, ok := v1.AWSToKubeArchitectures[string(image.Architecture)]
				if !ok {
					continue
				}
				// Each image may have multiple associated sets of requirements. For example, an image may be compatible with Neuron instances
				// and GPU instances. In that case, we'll have a set of requirements for each, and will create one "image" for each.
				for _, reqs := range query.RequirementsForImageWithArchitecture(lo.FromPtr(image.ImageId), arch) {
					// Checks and store for AMIs
					// Following checks are needed in order to always priortize non deprecated AMIs
					// If we already have an image with the same set of requirements, but this image (candidate) is newer, replace the previous (existing) image.
					// If we already have an image with the same set of requirements which is deprecated, but this image (candidate) is newer or non deprecated, replace the previous (existing) image
					reqsHash := lo.Must(hashstructure.Hash(reqs.NodeSelectorRequirements(), hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true}))
					candidateDeprecated := parseTimeWithDefault(lo.FromPtr(image.DeprecationTime), maxTime).Unix() <= p.clk.Now().Unix()
					ami := AMI{
						Name:         lo.FromPtr(image.Name),
						AmiID:        lo.FromPtr(image.ImageId),
						CreationDate: lo.FromPtr(image.CreationDate),
						Deprecated:   candidateDeprecated,
						Requirements: reqs,
					}
					if v, ok := queryImages[reqsHash]; ok {
						if cmpResult := compareAMI(v, ami); cmpResult <= 0 {
							continue
						}
					}
					queryImages[reqsHash] = ami
				}
			}
		}
		queryAMIs := AMIs(lo.Values(queryImages))
		p.cache.SetDefault(key, queryAMIs)
		mergeAMIs(images, queryAMIs)
	}
	return lo.Values(images), nil
}

// mergeAMIs merges the given AMIs into the accumulator keyed by requirements
// hash, keeping the preferred AMI (see compareAMI) when multiple AMIs share the
// same set of requirements. Merging per-query results this way is equivalent to
// resolving every query into a single map, since compareAMI defines a total
// order per requirements hash.
func mergeAMIs(into map[uint64]AMI, amis AMIs) {
	for i := range amis {
		reqsHash := lo.Must(hashstructure.Hash(amis[i].Requirements.NodeSelectorRequirements(), hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true}))
		if v, ok := into[reqsHash]; ok {
			if cmpResult := compareAMI(v, amis[i]); cmpResult <= 0 {
				continue
			}
		}
		into[reqsHash] = amis[i]
	}
}

// imageQueryCacheKey returns a deterministic, collision-free cache key for a
// DescribeImages query. The key incorporates both the resolved filters and the
// owners so that queries which differ only by owner (for example, an AMI name
// resolved against different accounts) never share a cache entry — critical to
// avoid returning cross-account AMIs the user did not authorize. For alias
// families the filters are SSM-resolved image-id filters, which uniquely
// identify the family and version, so owners and filters are sufficient to
// disambiguate queries without including KnownRequirements.
func imageQueryCacheKey(query DescribeImageQuery) string {
	owners := append([]string(nil), query.Owners...)
	sort.Strings(owners)
	return fmt.Sprintf("owners:%s\x1efilters:%s", strings.Join(owners, ","), utils.CanonicalFilterSetKey(query.Filters))
}

// MapToInstanceTypes returns a map of AMIIDs that are the most recent on creationDate to compatible instancetypes
func MapToInstanceTypes(instanceTypes []*cloudprovider.InstanceType, amis []v1.AMI) map[string][]*cloudprovider.InstanceType {
	amiIDs := map[string][]*cloudprovider.InstanceType{}
	for _, instanceType := range instanceTypes {
		for _, ami := range amis {
			if err := instanceType.Requirements.Compatible(
				scheduling.NewNodeSelectorRequirements(ami.Requirements...),
			); err == nil {
				amiIDs[ami.ID] = append(amiIDs[ami.ID], instanceType)
				break
			}
		}
	}
	return amiIDs
}

// Compare two AMI's based on their deprecation status, creation time or name
// If both AMIs are deprecated, compare creation time and return the one with the newer creation time
// If both AMIs are non-deprecated, compare creation time and return the one with the newer creation time
// If one AMI is deprecated, return the non deprecated one
// The result will be
// 0 if AMI i == AMI j, where creation date, deprecation status and name are all equal
// -1 if AMI i < AMI j, if AMI i is non-deprecated or newer than AMI j
// +1 if AMI i > AMI j, if AMI j is non-deprecated or newer than AMI i
func compareAMI(i, j AMI) int {
	iCreationDate := parseTimeWithDefault(i.CreationDate, minTime)
	jCreationDate := parseTimeWithDefault(j.CreationDate, minTime)
	// Prioritize non-deprecated AMIs over deprecated ones
	if i.Deprecated != j.Deprecated {
		return lo.Ternary(i.Deprecated, 1, -1)
	}
	// If both are either non-deprecated or deprecated, compare by creation date
	if iCreationDate.Unix() != jCreationDate.Unix() {
		return lo.Ternary(iCreationDate.Unix() > jCreationDate.Unix(), -1, 1)
	}
	// If they have the same creation date, use the name as a tie-breaker
	if i.Name != j.Name {
		return lo.Ternary(i.Name > j.Name, -1, 1)
	}
	// If all attributes are are equal, both AMIs are exactly identical
	return 0
}
