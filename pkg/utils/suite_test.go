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

package utils_test

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	"github.com/aws/karpenter-provider-aws/pkg/utils"
)

func TestUtils(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Utils Suite")
}

var _ = Describe("FilterSetHash", func() {
	filter := func(name string, values ...string) ec2types.Filter {
		return ec2types.Filter{Name: aws.String(name), Values: values}
	}
	hash := func(filters ...ec2types.Filter) uint64 {
		return utils.FilterSetHash(filters)
	}
	It("is independent of the order of filters and of values within a filter", func() {
		Expect(hash(filter("tag:a", "1", "2"), filter("tag:b", "3"))).
			To(Equal(hash(filter("tag:b", "3"), filter("tag:a", "2", "1"))))
	})
	It("ignores duplicates, which select the same resources", func() {
		Expect(hash(filter("subnet-id", "s-1", "s-1"))).To(Equal(hash(filter("subnet-id", "s-1"))))
		Expect(hash(filter("subnet-id", "s-1", "s-2", "s-2"))).To(Equal(hash(filter("subnet-id", "s-1", "s-2"))))
		Expect(hash(filter("tag:a", "1"), filter("tag:a", "1"))).To(Equal(hash(filter("tag:a", "1"))))
	})
	// SlicesAsSets XORs set elements, so without the dedup in FilterSetHash every filter
	// set below would hash alike and a cache hit would return the wrong resources (#8619)
	It("distinguishes filter sets that differ only by a duplicated value", func() {
		Expect(lo.Uniq([]uint64{
			hash(filter("subnet-id", "s-1", "s-1")),
			hash(filter("subnet-id", "s-2", "s-2")),
			hash(filter("subnet-id")),
			hash(filter("subnet-id", "s-1", "s-2", "s-2")),
		})).To(HaveLen(4))
	})
	It("distinguishes AND (one filter set) from OR (separate filter sets)", func() {
		Expect(lo.Uniq([]uint64{
			hash(filter("tag:a", "1"), filter("tag:b", "2")),
			hash(filter("tag:a", "1")),
			hash(filter("tag:b", "2")),
		})).To(HaveLen(3))
	})
	It("distinguishes different filter values", func() {
		Expect(hash(filter("tag:a", "1"))).ToNot(Equal(hash(filter("tag:a", "2"))))
	})
})
