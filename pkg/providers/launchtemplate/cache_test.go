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

package launchtemplate

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"

	sdk "github.com/aws/karpenter-provider-aws/pkg/aws"
	"github.com/aws/karpenter-provider-aws/pkg/operator/options"
)

// launchTemplateEC2API serves DescribeLaunchTemplates from fixed pages and records DeleteLaunchTemplate calls
type launchTemplateEC2API struct {
	sdk.EC2API

	mu           sync.Mutex
	pages        [][]ec2types.LaunchTemplate
	describeErr  error
	describes    []*ec2.DescribeLaunchTemplatesInput
	deleteErr    error
	deletedLTIDs []string
}

func (a *launchTemplateEC2API) DescribeLaunchTemplates(_ context.Context, input *ec2.DescribeLaunchTemplatesInput, _ ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.describes = append(a.describes, input)
	if a.describeErr != nil {
		return nil, a.describeErr
	}
	page := len(a.describes) - 1
	out := &ec2.DescribeLaunchTemplatesOutput{LaunchTemplates: a.pages[page]}
	if page < len(a.pages)-1 {
		out.NextToken = aws.String("next")
	}
	return out, nil
}

func (a *launchTemplateEC2API) DeleteLaunchTemplate(_ context.Context, input *ec2.DeleteLaunchTemplateInput, _ ...func(*ec2.Options)) (*ec2.DeleteLaunchTemplateOutput, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deletedLTIDs = append(a.deletedLTIDs, aws.ToString(input.LaunchTemplateId))
	return &ec2.DeleteLaunchTemplateOutput{}, a.deleteErr
}

func (a *launchTemplateEC2API) deleted() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.deletedLTIDs...)
}

func launchTemplate(name, id string) ec2types.LaunchTemplate {
	return ec2types.LaunchTemplate{LaunchTemplateName: aws.String(name), LaunchTemplateId: aws.String(id)}
}

var _ = Describe("LaunchTemplate Cache", func() {
	var ctx context.Context
	var ec2api *launchTemplateEC2API
	var ltCache *cache.Cache
	var provider *DefaultProvider

	BeforeEach(func() {
		ctx = options.ToContext(context.Background(), &options.Options{ClusterName: "test-cluster"})
		ec2api = &launchTemplateEC2API{}
		// No janitor, so entries only leave the cache when a spec removes them
		ltCache = cache.New(time.Hour, cache.NoExpiration)
		provider = &DefaultProvider{ec2api: ec2api, cache: ltCache}
		ltCache.OnEvicted(provider.cachedEvictedFunc(ctx))
	})

	Context("hydrateCache", func() {
		It("should add every launch template tagged for the cluster across pages", func() {
			ec2api.pages = [][]ec2types.LaunchTemplate{
				{launchTemplate("karpenter.k8s.aws/1", "lt-1"), launchTemplate("karpenter.k8s.aws/2", "lt-2")},
				{launchTemplate("karpenter.k8s.aws/3", "lt-3")},
			}
			provider.hydrateCache(ctx)

			Expect(ec2api.describes).To(HaveLen(2))
			Expect(ec2api.describes[0].Filters).To(ConsistOf(ec2types.Filter{
				Name:   aws.String("tag:eks:eks-cluster-name"),
				Values: []string{"test-cluster"},
			}))
			Expect(lo.FromPtr(ec2api.describes[0].MaxResults)).To(BeEquivalentTo(200))
			Expect(ec2api.describes[1].NextToken).To(Equal(aws.String("next")))

			Expect(ltCache.ItemCount()).To(Equal(3))
			for _, name := range []string{"karpenter.k8s.aws/1", "karpenter.k8s.aws/2", "karpenter.k8s.aws/3"} {
				lt, ok := ltCache.Get(name)
				Expect(ok).To(BeTrue(), name)
				Expect(lt.(ec2types.LaunchTemplate).LaunchTemplateName).To(Equal(aws.String(name)))
			}
		})
		It("should stop hydrating without caching anything when describing launch templates fails", func() {
			ec2api.describeErr = errors.New("throttled")
			provider.hydrateCache(ctx)
			Expect(ec2api.describes).To(HaveLen(1))
			Expect(ltCache.ItemCount()).To(BeZero())
		})
	})
	Context("cachedEvictedFunc", func() {
		It("should delete a launch template from EC2 by ID when it expires from the cache", func() {
			ltCache.Set("karpenter.k8s.aws/1", launchTemplate("karpenter.k8s.aws/1", "lt-1"), time.Nanosecond)
			ltCache.SetDefault("karpenter.k8s.aws/2", launchTemplate("karpenter.k8s.aws/2", "lt-2"))

			Eventually(func() []string {
				ltCache.DeleteExpired()
				return ec2api.deleted()
			}).Should(Equal([]string{"lt-1"}))
			_, ok := ltCache.Get("karpenter.k8s.aws/2")
			Expect(ok).To(BeTrue())
		})
		It("should not delete a launch template that was cached again before the eviction ran", func() {
			lt := launchTemplate("karpenter.k8s.aws/1", "lt-1")
			// EnsureAll can re-cache the launch template between the cache expiring it and calling OnEvicted
			ltCache.SetDefault("karpenter.k8s.aws/1", lt)
			provider.cachedEvictedFunc(ctx)("karpenter.k8s.aws/1", lt)
			Expect(ec2api.deleted()).To(BeEmpty())
		})
		It("should delete a launch template whose cached entry has already expired", func() {
			lt := launchTemplate("karpenter.k8s.aws/1", "lt-1")
			ltCache.OnEvicted(nil)
			ltCache.Set("karpenter.k8s.aws/1", lt, time.Nanosecond)
			Eventually(func() bool {
				_, ok := ltCache.Get("karpenter.k8s.aws/1")
				return ok
			}).Should(BeFalse())
			provider.cachedEvictedFunc(ctx)("karpenter.k8s.aws/1", lt)
			Expect(ec2api.deleted()).To(Equal([]string{"lt-1"}))
		})
		It("should tolerate a launch template that EC2 has already deleted", func() {
			ec2api.deleteErr = &smithy.GenericAPIError{Code: "InvalidLaunchTemplateId.NotFound"}
			lt := launchTemplate("karpenter.k8s.aws/1", "lt-1")
			Expect(func() { provider.cachedEvictedFunc(ctx)("karpenter.k8s.aws/1", lt) }).ToNot(Panic())
			Expect(ec2api.deleted()).To(Equal([]string{"lt-1"}))
		})
	})
	Context("InvalidateCache", func() {
		It("should drop the launch template from the cache without deleting it from EC2", func() {
			ltCache.SetDefault("karpenter.k8s.aws/1", launchTemplate("karpenter.k8s.aws/1", "lt-1"))
			provider.InvalidateCache(ctx, "karpenter.k8s.aws/1", "lt-1")

			_, ok := ltCache.Get("karpenter.k8s.aws/1")
			Expect(ok).To(BeFalse())
			Expect(ec2api.deleted()).To(BeEmpty())

			// Eviction is restored for later entries
			ltCache.Set("karpenter.k8s.aws/2", launchTemplate("karpenter.k8s.aws/2", "lt-2"), time.Nanosecond)
			Eventually(func() []string {
				ltCache.DeleteExpired()
				return ec2api.deleted()
			}).Should(Equal([]string{"lt-2"}))
		})
	})
})
