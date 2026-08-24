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

	"github.com/aws/karpenter-provider-aws/pkg/utils"
)

func TestUtils(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Utils Suite")
}

var _ = Describe("CanonicalFilterSetKey", func() {
	filter := func(name string, values ...string) ec2types.Filter {
		return ec2types.Filter{Name: aws.String(name), Values: values}
	}
	It("is independent of the order of filters and of values within a filter", func() {
		a := utils.CanonicalFilterSetKey([]ec2types.Filter{filter("tag:a", "1", "2"), filter("tag:b", "3")})
		b := utils.CanonicalFilterSetKey([]ec2types.Filter{filter("tag:b", "3"), filter("tag:a", "2", "1")})
		Expect(a).To(Equal(b))
	})
	It("distinguishes AND (one filter set) from OR (separate filter sets)", func() {
		and := utils.CanonicalFilterSetKey([]ec2types.Filter{filter("tag:a", "1"), filter("tag:b", "2")})
		orA := utils.CanonicalFilterSetKey([]ec2types.Filter{filter("tag:a", "1")})
		orB := utils.CanonicalFilterSetKey([]ec2types.Filter{filter("tag:b", "2")})
		Expect(and).ToNot(Equal(orA))
		Expect(and).ToNot(Equal(orB))
		Expect(orA).ToNot(Equal(orB))
	})
	It("distinguishes different filter values", func() {
		Expect(utils.CanonicalFilterSetKey([]ec2types.Filter{filter("tag:a", "1")})).
			ToNot(Equal(utils.CanonicalFilterSetKey([]ec2types.Filter{filter("tag:a", "2")})))
	})
	It("does not collide on filter sets containing duplicate values (unlike SlicesAsSets)", func() {
		dup := utils.CanonicalFilterSetKey([]ec2types.Filter{filter("tag:a", "1", "1")})
		other := utils.CanonicalFilterSetKey([]ec2types.Filter{filter("tag:b", "2", "2")})
		Expect(dup).ToNot(Equal(other))
		Expect(dup).ToNot(BeEmpty())
	})
})
