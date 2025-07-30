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

package aws

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/timestreamwrite"
	"github.com/samber/lo"

	sdk "github.com/aws/karpenter-provider-aws/pkg/aws"

	"github.com/aws/karpenter-provider-aws/test/pkg/environment/common"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	metricsDefaultRegion = "us-east-2"
	databaseName         = "karpenterTesting"
	tableName            = "scaleTestDurations"
	metricsNamespace     = "karpenterTesting"
)

type NoOpTimeStreamAPI struct {
	sdk.TimestreamWriteAPI
}

func (o NoOpTimeStreamAPI) WriteRecords(_ context.Context, _ *timestreamwrite.WriteRecordsInput, _ ...func(*timestreamwrite.Options)) (*timestreamwrite.WriteRecordsOutput, error) {
	return nil, nil
}

type NoOpCloudWatchAPI struct {
	sdk.CloudWatchAPI
}

func (o NoOpCloudWatchAPI) PutMetricData(_ context.Context, _ *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
	return nil, nil
}

type EventType string

const (
	ProvisioningEventType   EventType = "provisioning"
	DeprovisioningEventType EventType = "deprovisioning"
)

const (
	TestCategoryDimension           = "category"
	TestNameDimension               = "name"
	GitRefDimension                 = "gitRef"
	ProvisionedNodeCountDimension   = "provisionedNodeCount"
	DeprovisionedNodeCountDimension = "deprovisionedNodeCount"
	PodDensityDimension             = "podDensity"
)

func (env *Environment) MeasureProvisioningDurationFor(f func(), dimensions map[string]string) {
	GinkgoHelper()

	env.MeasureDurationFor(f, ProvisioningEventType, dimensions)
}

func (env *Environment) MeasureDeprovisioningDurationFor(f func(), dimensions map[string]string) {
	GinkgoHelper()

	env.MeasureDurationFor(f, DeprovisioningEventType, dimensions)
}

// MeasureDurationFor observes the duration between the beginning of the function f() and the end of the function f()
func (env *Environment) MeasureDurationFor(f func(), eventType EventType, dimensions map[string]string) {
	GinkgoHelper()
	start := time.Now()
	f()
	gitRef := "n/a"
	if env.Context.Value(common.GitRefContextKey) != nil {
		gitRef = env.Value(common.GitRefContextKey).(string)
	}

	dimensions = lo.Assign(dimensions, map[string]string{
		GitRefDimension: gitRef,
	})
	switch eventType {
	case ProvisioningEventType:
		env.ExpectMetric("provisioningDuration", time.Since(start).Seconds(), dimensions)
	case DeprovisioningEventType:
		env.ExpectMetric("deprovisioningDuration", time.Since(start).Seconds(), dimensions)
	}
}

func (env *Environment) ExpectMetric(name string, value float64, labels map[string]string) {
	GinkgoHelper()
	_, err := env.CloudWatchAPI.PutMetricData(env.Context, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(metricsNamespace),
		MetricData: []cloudwatchtypes.MetricDatum{
			{
				MetricName: aws.String(name),
				Value:      aws.Float64(value),
				Dimensions: lo.MapToSlice(labels, func(k, v string) cloudwatchtypes.Dimension {
					return cloudwatchtypes.Dimension{
						Name:  aws.String(k),
						Value: aws.String(v),
					}
				}),
				Timestamp: aws.Time(time.Now()),
				Unit:      cloudwatchtypes.StandardUnitSeconds,
			},
		},
	})
	Expect(err).ToNot(HaveOccurred())
}
