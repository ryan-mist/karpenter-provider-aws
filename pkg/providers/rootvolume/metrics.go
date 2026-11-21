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
	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	awsSubsystem = "aws"
	outcomeLabel = "outcome"

	OutcomeSucceeded      = "succeeded"
	OutcomeFailed         = "failed"
	OutcomeFailedDetached = "failed_detached"
)

var Outcome = opmetrics.Label{
	Name: outcomeLabel,
	Help: "The terminal state of the replace root volume task. See https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_ReplaceRootVolumeTask.html.",
	Values: []opmetrics.Value{
		{Name: OutcomeSucceeded, Help: "The replacement root volume is attached and the instance is available."},
		{Name: OutcomeFailed, Help: "The task failed and the original root volume is still attached."},
		{Name: OutcomeFailedDetached, Help: "The task failed and the instance has no root volume attached."},
	},
}

var (
	// TasksTotal and TaskDurationSeconds are observed once per task, when the provider first sees it terminal. Tasks
	// that completed long before the provider first saw them, as when the cache hydrates after a restart, are skipped.
	TasksTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: awsSubsystem,
			Name:      "root_volume_replacement_tasks_total",
			Help:      "Number of replace root volume tasks Karpenter created that reached a terminal state, dimensioned by outcome.",
		},
		[]opmetrics.Label{Outcome},
		opmetrics.Alpha,
	)
	TaskDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: awsSubsystem,
			Name:      "root_volume_replacement_task_duration_seconds",
			Help:      "Time from a replace root volume task's start to its completion as reported by EC2, dimensioned by outcome.",
			// Tasks take minutes, past the 600s top of metrics.DurationBuckets
			Buckets: []float64{15, 30, 45, 60, 90, 120, 180, 240, 300, 450, 600, 900, 1200, 1800, 3600},
		},
		[]opmetrics.Label{Outcome},
		opmetrics.Alpha,
	)
)
