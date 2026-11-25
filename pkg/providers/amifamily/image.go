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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
)

// ImageTTL is how long image attributes are cached. The attributes Karpenter reads are fixed when the image is
// registered, so the TTL only bounds memory and picks up deregistration.
const ImageTTL = time.Hour

// missingImageTTL is how long Karpenter remembers that an image could not be described.
const missingImageTTL = 5 * time.Minute

// maxImageIDsPerCall bounds the number of values in one image-id filter.
const maxImageIDsPerCall = 100

// Image is the subset of DescribeImages output that decides whether an instance can move to the image in place.
type Image struct {
	ID              string
	Name            string
	CreationDate    string
	Platform        ec2types.PlatformValues
	BootMode        ec2types.BootModeValues
	EnaSupport      bool
	SriovNetSupport string
	TpmSupport      ec2types.TpmSupportValues
	ProductCodes    []string
	// RootSnapshotID is the snapshot behind the image's root device, or empty if the image has none.
	RootSnapshotID string
}

func NewImage(image ec2types.Image) *Image {
	rootDevice := lo.FromPtr(image.RootDeviceName)
	var rootSnapshotID string
	for _, bdm := range image.BlockDeviceMappings {
		if lo.FromPtr(bdm.DeviceName) == rootDevice && bdm.Ebs != nil {
			rootSnapshotID = lo.FromPtr(bdm.Ebs.SnapshotId)
			break
		}
	}
	return &Image{
		ID:              lo.FromPtr(image.ImageId),
		Name:            lo.FromPtr(image.Name),
		CreationDate:    lo.FromPtr(image.CreationDate),
		Platform:        image.Platform,
		BootMode:        image.BootMode,
		EnaSupport:      lo.FromPtr(image.EnaSupport),
		SriovNetSupport: lo.FromPtr(image.SriovNetSupport),
		TpmSupport:      image.TpmSupport,
		ProductCodes: lo.Map(image.ProductCodes, func(pc ec2types.ProductCode, _ int) string {
			return lo.FromPtr(pc.ProductCodeId)
		}),
		RootSnapshotID: rootSnapshotID,
	}
}

func (p *DefaultProvider) cacheImage(image ec2types.Image) {
	if id := lo.FromPtr(image.ImageId); id != "" {
		p.images.SetDefault(id, NewImage(image))
	}
}

func newImageCache() *cache.Cache {
	return cache.New(ImageTTL, ImageTTL)
}

// ResetImages drops cached image attributes.
func (p *DefaultProvider) ResetImages() {
	p.images.Flush()
}

// Images returns the attributes of the requested images, keyed by ID. Images EC2 does not return, for example
// because they were deregistered, are absent from the result. It reads the cache first and makes one
// DescribeImages call per batch of misses.
func (p *DefaultProvider) Images(ctx context.Context, ids ...string) (map[string]*Image, error) {
	out := map[string]*Image{}
	var misses []string
	for _, id := range lo.Uniq(ids) {
		if v, ok := p.images.Get(id); ok {
			if image := v.(*Image); image != nil {
				out[id] = image
			}
			continue
		}
		misses = append(misses, id)
	}
	for _, chunk := range lo.Chunk(misses, maxImageIDsPerCall) {
		paginator := ec2.NewDescribeImagesPaginator(p.ec2api, &ec2.DescribeImagesInput{
			Filters:           []ec2types.Filter{{Name: aws.String("image-id"), Values: chunk}},
			IncludeDeprecated: aws.Bool(true),
		})
		found := map[string]struct{}{}
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("describing images, %w", err)
			}
			for _, image := range page.Images {
				p.cacheImage(image)
				id := lo.FromPtr(image.ImageId)
				out[id] = NewImage(image)
				found[id] = struct{}{}
			}
		}
		for _, id := range chunk {
			if _, ok := found[id]; !ok {
				p.images.Set(id, (*Image)(nil), missingImageTTL)
			}
		}
	}
	return out, nil
}
