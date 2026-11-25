---
title: "Monitoring Karpenter"
linkTitle: "Monitoring Karpenter"
weight: 10
description: >
  Recommended service-level objectives, alerts, and dashboards for running Karpenter on AWS
---

Karpenter sits on the critical path of your cluster's capacity. When it can't launch nodes, pods stay
`Pending`. When it disrupts nodes too fast, or in a way you didn't expect, workloads lose capacity.
When it can't finish terminating nodes, you pay for idle instances and rollouts stall. Most of these
failures don't crash Karpenter. The controller keeps running, its health probes stay green, and the
first person to notice is usually a workload owner asking why their pods aren't running, sometimes
hours or days later.

This page describes a recommended monitoring setup for self-managed Karpenter on Amazon EC2:

* [What to collect](#collecting-signals): metrics, Kubernetes Events, and logs
* [Service-level objectives](#service-level-objectives) to track over time
* [Alerts](#alerts), split into alerts that should page someone and alerts that should open a ticket
* [Forceful termination after `terminationGracePeriod`](#detecting-forceful-termination-after-terminationgraceperiod), which is easy to forget and catches many misconfigurations
* [Metrics that are easy to misread](#metrics-that-are-easy-to-misread)
* [Failure modes that have no metric today](#failure-modes-without-a-direct-metric), and how to cover them
* [Operational practices](#operational-practices), for example what to watch after an upgrade

The queries on this page are PromQL. They use metrics documented in the [Metrics reference]({{< relref "../reference/metrics" >}}).
The thresholds are starting points: tune them to your cluster's size, scaling patterns, and
tolerance for noise. For Amazon EC2 API call volume and throttling, see also
[Monitoring Amazon EC2 API Usage]({{< relref "./monitoring-ec2-api-usage" >}}).

{{% alert title="Note" color="primary" %}}
This page is about self-managed Karpenter. In EKS Auto Mode, AWS runs Karpenter for you and its
`/metrics` endpoint and controller logs aren't available to you. The Kubernetes-side signals on this
page, such as pod `Pending` age, Node conditions, NodeClaim conditions and Events, still apply.
{{% /alert %}}

## Collecting signals

You need three kinds of signal. Metrics alone aren't enough: some important behaviors, including
forceful termination after `terminationGracePeriod`, are only reported as Kubernetes Events.

### Metrics

Karpenter serves Prometheus metrics over plain HTTP at `:8080/metrics` (set with `METRICS_PORT`). The
Helm chart creates a `Service` with a port named `http-metrics`. If you run the Prometheus Operator,
enable the chart's `ServiceMonitor`, which is off by default:

```yaml
serviceMonitor:
  enabled: true
  # additionalLabels, endpointConfig, metricRelabelings, relabelings, and sampleLimit are also supported
```

Keep these points in mind:

* **Only the leader does the work.** Every replica serves `/metrics`, but Karpenter's controllers
  only run on the elected leader, so Karpenter-specific metrics only move on the leader. Aggregate
  across pods with `sum` or `max` instead of alerting per pod. Where a stale series from a former
  leader could mask a problem, join on `leader_election_master_status == 1`.
* **Don't disable cluster state observability if you want these alerts.** Setting
  `settings.disableClusterStateObservability` (`DISABLE_CLUSTER_STATE_OBSERVABILITY`) turns off the
  pod, node, and NodePool metrics controllers and **all** `operator_*` status-condition metrics.
  Several alerts on this page depend on them.
* **Manage cardinality instead of turning off scraping.** The largest series counts come from
  `karpenter_cloudprovider_instance_type_offering_available`,
  `karpenter_cloudprovider_instance_type_offering_price_estimate` (one series per instance type,
  capacity type, and zone), `karpenter_pods_state` (one series per pod), and the `karpenter_nodes_*`
  gauges (one series per node and resource). If they're too expensive, drop them with
  `metricRelabelings` rather than disabling scraping and losing your alerts.
* **If you export to Amazon CloudWatch**, counters typically arrive as per-period deltas. Use the
  `Sum` statistic, not `Max` or `Average`. Make sure the cluster name is a dimension, so that alarms
  from several clusters don't merge.

Also collect the standard [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics)
and cAdvisor metrics for the Karpenter Deployment and pods: restarts, `OOMKilled` terminations, CPU
throttling, and memory working set relative to the limit.

### Kubernetes Events

Karpenter reports many decisions only as Events on Pods, Nodes, NodeClaims, and NodePools:

| Event reason | Object | Meaning |
|---|---|---|
| `Disrupted` (with message `... This bypasses the PDB of the pod and the do-not-disrupt annotation.`) | Pod | Karpenter deleted the pod because its node's `terminationGracePeriod` was about to expire. See [below](#detecting-forceful-termination-after-terminationgraceperiod). |
| `TerminationGracePeriodExpiring` | Node, NodeClaim | A termination deadline is set: "All pods will be deleted by `<time>`". |
| `FailedDraining` | Node | Draining is blocked, for example because evicting a pod would violate a PDB. |
| `DisruptionBlocked`, `Unconsolidatable` | Node, NodeClaim, NodePool | Karpenter wanted to disrupt a node but couldn't, for example because of `karpenter.sh/do-not-disrupt`, PDBs, or a budget. |
| `InsufficientCapacityError` | NodeClaim, Pod | A launch failed because no requested offering had capacity. |
| `NodeClassNotReady` | NodeClaim | A launch was skipped because the NodeClaim's EC2NodeClass isn't ready. |

Events expire from the API server after an hour by default, so export them to a durable store with
an event exporter, the Amazon CloudWatch Container Insights event pipeline, or your log pipeline,
and alert on them there. Karpenter emits Node and NodeClaim events in the `default` namespace, so
route them to the platform team, not to workload owners.

### Logs

Ship Karpenter controller logs to a durable store, such as Amazon CloudWatch Logs, with retention of
at least a few weeks. Logs often rotate before an investigation starts. Karpenter logs at `info`
by default. Some useful lines, such as the per-pod force-delete message (`deleting pod`) and
liveness timeouts (`terminating due to timeout`), are only logged at `debug`. Know how to switch the
log level (`logLevel` in the Helm chart) before you need to.

Useful log-based alerts:

* `karpenter is not compatible with kubernetes version`: the controller keeps running and looks
  healthy, but won't work correctly on this cluster version.
* `cluster is waiting on sync for extended duration`: see [cluster state not synced](#critical-alerts).
* `exceed limits for nodepool`: provisioning is blocked by NodePool `limits`.
* Any `ERROR` line mentioning `UnauthorizedOperation`, `AccessDenied`, or `AuthFailure`.

## Service-level objectives

Track these indicators over time. They tell you whether Karpenter is doing its job, and they're the
basis for the alerts in the next section. The targets are examples; set your own based on your
workloads.

| Objective | Indicator | Example target | How to measure |
|---|---|---|---|
| **Provisioning latency** | Time from when Karpenter decides a pod can be scheduled to when the pod is bound | p99 < 3 min | `histogram_quantile(0.99, sum by (le) (rate(karpenter_pods_provisioning_bound_duration_seconds_bucket[1h])))` |
| **Pod startup** | Time from pod creation to `Running`, for pods that needed new capacity | p99 < 5 min | `karpenter_pods_provisioning_startup_duration_seconds` (histogram) |
| **Launch success** | NodeClaims that launch, register, and initialize, out of all NodeClaims created | > 99% | `1 - sum(increase(karpenter_nodeclaims_disrupted_total{reason=~"launch_timeout\|registration_timeout\|insufficient_capacity\|nodeclass_not_ready"}[1d])) / sum(increase(karpenter_nodeclaims_created_total[1d]))` |
| **Controller availability** | Fraction of time a Karpenter leader exists | > 99.9% | `max(leader_election_master_status{name="karpenter-leader-election"})` averaged over time |
| **State consistency** | Fraction of time Karpenter's cluster state is synced | > 99.9% | `max(karpenter_cluster_state_synced)` averaged over time |
| **Safe disruption** | Pods deleted without respecting their PDB or `do-not-disrupt` | 0 (unless you accept forced termination by design) | Count of `Disrupted` Pod Events; see [below](#detecting-forceful-termination-after-terminationgraceperiod) |
| **Termination time** | Time from NodeClaim deletion to the instance being gone | p99 < your `terminationGracePeriod`, or < 1 h if none | `histogram_quantile(0.99, sum by (le, nodepool) (rate(karpenter_nodeclaims_termination_duration_seconds_bucket[1d])))` |
| **Disruption pace** | Nodes disrupted per NodePool per window, compared with your budgets | Within budget | `sum by (nodepool, reason) (increase(karpenter_nodeclaims_disrupted_total[1h]))` |

## Alerts

The alerts are grouped by urgency. Critical alerts mean capacity is or will shortly be affected and
should page someone. Warning alerts point at misconfiguration, waste, or degradation that should be
fixed within a working day.

{{% alert title="Note" color="primary" %}}
If you change `LEADER_ELECTION_NAME` from its default, use your value in place of
`karpenter-leader-election` below.
{{% /alert %}}

### Critical alerts

| Alert | Query | For | What it catches |
|---|---|---|---|
| **Pods pending, no capacity being created** | `(max(karpenter_pods_provisioning_unbound_time_seconds) > 600) and on() (sum(increase(karpenter_nodeclaims_created_total[15m])) == 0)` | 5m | The most common customer-impacting failure: pods wait while Karpenter creates nothing, for example because of an invalid requirement, NodePool limits, an unsatisfiable constraint, or a stuck controller. Also alert on `sum(karpenter_scheduler_unschedulable_pods_count{controller="provisioner"}) > 0` for 15m. |
| **No Karpenter leader** | `max(leader_election_master_status{name="karpenter-leader-election"}) < 1 or absent(leader_election_master_status{name="karpenter-leader-election"})` | 2m | Lost leases (for example from API server throttling or CPU starvation), crash loops, or replicas scaled to 0. |
| **Karpenter metrics missing** | `absent(karpenter_build_info)` | 5m | Karpenter isn't running or isn't being scraped. Without this alert, every other alert silently stops firing. |
| **Cluster state not synced** | `max(karpenter_cluster_state_unsynced_time_seconds) > 600` | 0m | Karpenter's in-memory view doesn't match the API server. While unsynced, Karpenter **stops provisioning and disrupting** but stays up and reports healthy probes. |
| **Controller panics** | `sum(increase(controller_runtime_reconcile_panics_total[10m])) > 0` | 0m | Code paths crashing on specific inputs. Check the logs for the stack trace. |
| **Controller crash-looping or OOMKilled** | `increase(kube_pod_container_status_restarts_total{namespace="<karpenter-namespace>", container="controller"}[15m]) > 2` | 0m | Out-of-memory after upgrades or growth, bad configuration, or missing permissions. Also alert when memory use exceeds 85% of the limit. |
| **NodePool can't register nodes** | `max by (name) (operator_nodepool_status_condition_count{type="NodeRegistrationHealthy", status="False"}) > 0` | 5m | At least half of this NodePool's recent launches failed to launch or register, for example because of a bad AMI, user data, instance profile, network path, or impaired Availability Zone. |
| **EC2NodeClass or NodePool not ready** | `max by (name) (operator_ec2nodeclass_status_condition_count{type="Ready", status="False"}) > 0` and the same for `operator_nodepool_status_condition_count` | 5m | Karpenter can't launch from this NodePool or EC2NodeClass: missing subnets, security groups, AMIs, or instance profile, or a validation failure. |
| **Launches failing on permissions** | `sum by (reason) (increase(karpenter_cloudprovider_instance_launch_failures_total{reason=~"Unauthorized\|AMIAuthorizationFailure\|InstanceProfileNameInvalid"}[15m])) > 0` | 0m | SCPs, VPC endpoint policies, or IAM changes blocking `CreateFleet`. These never recover on their own. |
| **Disruption loop wedged** | `max(workqueue_unfinished_work_seconds{name="disruption"}) > 1800` | 0m | The disruption controller has been stuck in one pass for 30 minutes. Drift, expiration-driven replacements, consolidation, and repair all stop, with no other symptom. |

### Warning alerts

#### Provisioning and launches

| Alert | Query | For | What it catches |
|---|---|---|---|
| **Insufficient capacity** | `sum by (nodepool) (increase(karpenter_cloudprovider_errors_total{method="Create", error="InsufficientCapacityError"}[15m])) > 0` | 30m | Sustained ICE for a NodePool. Widen instance type, Availability Zone, or capacity type flexibility. Pair with `count by (zone, capacity_type) (karpenter_cloudprovider_instance_type_offering_available == 0)` to see which offerings are exhausted. |
| **Launch failures by EC2 reason** | `sum by (reason) (increase(karpenter_cloudprovider_instance_launch_failures_total[15m])) > 0` | 15m | Quota (`VCPULimitExceeded`, `SpotQuotaExceeded`, `FleetQuotaExceeded`), subnet IP exhaustion (`InsufficientFreeAddressesInSubnet`), AMI and launch template problems. |
| **Launch or registration timeouts** | `sum by (nodepool, reason) (increase(karpenter_nodeclaims_disrupted_total{reason=~"launch_timeout\|registration_timeout"}[30m])) > 0` | 0m | Instances that never launched (5 minutes) or never joined the cluster (15 minutes). Repeated timeouts usually precede `NodeRegistrationHealthy=False`. |
| **NodeClaims not initialized** | `max by (name) (operator_nodeclaim_status_condition_current_status_seconds{type="Initialized", status!="True"}) > 900` | 0m | Nodes that registered but never became initialized, for example because a startup taint was never removed, a CNI or DRA driver didn't become ready, or extended resources never appeared. There's no timeout for this, so these NodeClaims can stay forever. |
| **NodePool approaching limits** | `max by (nodepool, resource_type) (karpenter_nodepools_usage / on(nodepool, resource_type) karpenter_nodepools_limit) > 0.9` | 15m | Provisioning will soon stop for this NodePool. |
| **Slow scheduling simulations** | `histogram_quantile(0.99, sum by (le) (rate(karpenter_scheduler_scheduling_duration_seconds_bucket{controller="provisioner"}[10m]))) > 30` | 15m | Scheduling simulations approaching their timeout, typically on large or highly constrained clusters, or when the controller is CPU-limited. |
| **Cloud provider calls slow** | `histogram_quantile(0.99, sum by (le, method) (rate(karpenter_cloudprovider_duration_seconds_bucket[10m]))) > 30` | 15m | Hung or slow AWS calls. These show up as latency, not errors, in `karpenter_cloudprovider_errors_total`. |
| **Unexpected cloud provider errors** | `sum by (method) (rate(karpenter_cloudprovider_errors_total{error="unknown"}[5m])) > 0` | 15m | Errors other than capacity, missing NodeClaims, or NodeClass readiness. Check the logs. |
| **EC2 API throttling** | `sum(rate(aws_sdk_go_request_attempt_total{service="EC2", code="503"}[5m])) / sum(rate(aws_sdk_go_request_attempt_total{service="EC2"}[5m])) > 0.05` | 15m | `RequestLimitExceeded` throttling. Use `aws_sdk_go_request_attempt_total`: it counts every attempt, so it includes throttles that later succeeded on retry. See [Monitoring Amazon EC2 API Usage]({{< relref "./monitoring-ec2-api-usage" >}}). |
| **Kubernetes API throttling** | `sum(rate(client_go_request_total{code="429"}[5m])) > 0` | 15m | API Priority and Fairness rejecting Karpenter's requests. Sustained throttling can cost Karpenter its leader lease. |

#### Disruption and termination

| Alert | Query | For | What it catches |
|---|---|---|---|
| **Pods force-deleted after `terminationGracePeriod`** | Event-based; see [below](#detecting-forceful-termination-after-terminationgraceperiod) | 0m | PDBs that can never be satisfied, `do-not-disrupt` pods on expiring nodes, or a `terminationGracePeriod` that's too short. |
| **NodeClaim stuck terminating** | `max by (karpenter_sh_nodepool) (operator_nodeclaim_termination_current_time_seconds) > 3600` | 0m | Drains blocked by PDBs or `do-not-disrupt` with no `terminationGracePeriod`, volumes that never detach, or instances stuck shutting down. Set the threshold just below your `terminationGracePeriod` to get an early warning before forced deletion. Alert on `operator_node_termination_current_time_seconds` the same way. |
| **Evictions blocked by PDBs** | `sum(increase(karpenter_pods_eviction_requests_total{code="429"}[15m])) > 0` | 1h | Evictions rejected by a PodDisruptionBudget for a long time. Find the PDBs with `disruptionsAllowed: 0`. |
| **Mass disruption** | `sum by (nodepool) (increase(karpenter_nodeclaims_disrupted_total{reason=~"drifted\|expired\|underutilized\|empty"}[15m])) / count by (nodepool) (karpenter_nodes_current_lifetime_seconds) > 0.2` | 0m | More than 20% of a NodePool replaced in 15 minutes, for example from a new AMI release, a NodeClass change, or expiration. `expired` isn't limited by disruption budgets. |
| **Forced terminations from EC2 health** | `sum by (nodepool, reason) (increase(karpenter_nodeclaims_disrupted_total{reason=~"instance_status\|system_status"}[1h])) > 0` | 0m | Instances that failed EC2 status checks and were terminated without a graceful drain. |
| **Disruption failing to execute** | `sum by (reason) (increase(karpenter_voluntary_disruption_queue_failures_total[30m])) > 0` | 0m | Replacement nodes for a drift or consolidation action failed to launch or initialize. |
| **Disruption blocked by budget** | `(max by (nodepool, reason) (karpenter_nodepools_allowed_disruptions) == 0) and on(reason) (max by (reason) (karpenter_voluntary_disruption_eligible_nodes) > 0)` | 6h | Drift or consolidation can't progress because the budget allows no disruptions. This metric can be missing or stale for reasons that have no candidates, so treat it as a hint and confirm with `DisruptionBlocked` events. |
| **Consolidation timeouts** | `sum by (consolidation_type) (increase(karpenter_voluntary_disruption_consolidation_timeouts_total[1h])) > 10` | 1h | Consolidation can't finish its search, so savings are missed. |
| **Node churn** | `histogram_quantile(0.5, sum by (le, nodepool) (rate(karpenter_nodes_lifetime_duration_seconds_bucket[1h]))) < 900` | 1h | Nodes living less than 15 minutes, for example from provision-then-consolidate loops, autoscaler fights, or short `consolidateAfter`. |
| **Node expiration approaching** | `max by (nodepool) (karpenter_nodes_current_lifetime_seconds{nodepool="<nodepool>"}) > 0.9 * <expireAfter in seconds>` | 0m | Advance warning before nodes expire. Expiration is forceful and doesn't respect disruption budgets. |

#### Node health

| Alert | Query | For | What it catches |
|---|---|---|---|
| **Node not ready** | `max by (name, karpenter_sh_nodepool) (operator_node_status_condition_current_status_seconds{type="Ready", status!="True"}) > 1800` | 0m | Nodes `NotReady` for more than 30 minutes that [Node Auto Repair]({{< relref "../concepts/disruption#node-auto-repair" >}}) hasn't replaced, either because repair is disabled or because something blocks it. |
| **Inconsistent NodeClaims** | `max by (name) (operator_nodeclaim_status_condition_count{type="ConsistentStateFound", status="False"}) > 0` | 15m | The Node and its NodeClaim disagree, for example a Node whose capacity doesn't match the instance type. |

#### Interruption handling

These apply if you configure `settings.interruptionQueue`.

| Alert | Query | For | What it catches |
|---|---|---|---|
| **Interruption messages handled late** | `histogram_quantile(0.99, sum by (le) (rate(karpenter_interruption_message_queue_duration_seconds_bucket[10m]))) > 60` | 15m | Spot interruption warnings arrive two minutes before reclaim. If Karpenter takes longer than a minute to process them, it can't drain in time. |
| **Interruption queue not draining** | `(sum(rate(karpenter_interruption_received_messages_total[30m])) > 0) and on() (sum(rate(karpenter_interruption_deleted_messages_total[30m])) == 0)` | 30m | Messages are received but never deleted, for example because of a missing `sqs:DeleteMessage` permission. |

### AWS-side signals

Some failures are only visible from AWS. Alarm on these with Amazon CloudWatch:

* **Subnet IP exhaustion**: available IPv4 addresses in the subnets your EC2NodeClasses select.
* **Service quotas**: Running On-Demand and Spot instance vCPU usage (`AWS/Usage` metrics) above 80% of the quota.
* **Reserved capacity**: Capacity Reservation `InstanceUtilization` dropping after a Karpenter
  upgrade or configuration change. A silent fallback to on-demand shows up only here and in your bill.
* **AWS CloudTrail**: `errorCode` values `UnauthorizedOperation`, `AccessDenied`, `Client.RequestLimitExceeded`
  for calls made by the Karpenter controller role.
* **AWS Health**: events for EC2 and your Availability Zones. Correlate them with `NotReady` nodes
  concentrated in one zone.

## Detecting forceful termination after `terminationGracePeriod`

When a NodeClaim has `spec.terminationGracePeriod`, Karpenter stops waiting on PodDisruptionBudgets
and `karpenter.sh/do-not-disrupt` once the grace period is about to run out. Each remaining pod is
deleted directly, not evicted, at the latest time that still gives it its full
`terminationGracePeriodSeconds` before the node's deadline. This is by design: it guarantees nodes eventually
terminate, for example when they expire or drift. But if it happens regularly, you probably have a
misconfiguration, such as:

* A PDB that can never be satisfied (for example `minAvailable` equal to the replica count, or
  `maxUnavailable: 0`).
* Long-running pods with `karpenter.sh/do-not-disrupt` on nodes that expire.
* A `terminationGracePeriod` shorter than the time your workloads actually need to drain.

**No metric counts these force deletions.** Detect them with the Pod Event Karpenter publishes for
each one:

* `type`: `Normal`
* `reason`: `Disrupted`
* `involvedObject.kind`: `Pod`
* `message`: `Deleting the pod to accommodate the terminationTime <time> of the node. The pod was granted <n> seconds of grace-period of its <m> terminationGracePeriodSeconds. This bypasses the PDB of the pod and the do-not-disrupt annotation.`

Alert when the count of these events, filtered on `bypasses the PDB` in the message, is greater than
0 over your window. To check by hand:

```bash
kubectl get events -A --field-selector reason=Disrupted,involvedObject.kind=Pod
```

For an earlier warning, before any pod is force-deleted:

* Alert when a NodeClaim has been terminating for most of its grace period:
  `max by (name, karpenter_sh_nodepool) (operator_nodeclaim_termination_current_time_seconds) > 0.8 * <terminationGracePeriod in seconds>`.
* Watch for `TerminationGracePeriodExpiring` and `FailedDraining` Warning events on Nodes, and
  `karpenter_pods_eviction_requests_total{code="429"}` (evictions blocked by PDBs).
* The NodeClaim annotation `karpenter.sh/nodeclaim-termination-timestamp` holds the exact deadline.

Related signals that cover only part of the picture:

* `operator_nodeclaim_status_condition_transitions_total{type="VolumesDetached", status="False", reason="TerminationGracePeriodElapsed"}`
  increments only when the grace period elapsed **and** volumes were still attached.
* `karpenter_nodeclaims_disrupted_total{reason=~"instance_status|system_status"}` counts nodes that
  EC2 health checks forced Karpenter to terminate immediately, with no graceful drain at all.

## Metrics that are easy to misread

* **`karpenter_pods_drained_total{reason="Forceful Termination"}` does not mean pods were
  force-deleted.** The `reason` label is the NodeClaim's disruption reason (`Drifted`,
  `Underutilized`, `Empty`, `Unhealthy`). `Forceful Termination` means only that the NodeClaim had no
  voluntary disruption reason, for example because it expired, was interrupted, or was deleted by
  hand. Those pods are usually evicted gracefully. A drifted node whose grace period expired reports
  its force-deleted pods as `Drifted`.
* **`termination_mode="eventual"` doesn't mean a force deletion happened.** On
  `karpenter_nodeclaims_disrupted_total` and `karpenter_pods_disruption_initiated_total`, it only says
  the NodeClaim has a `terminationGracePeriod`, so it *could* be force-terminated. `forceful` means
  the grace period was zero or negative.
* **Insufficient capacity has no reason of its own in `karpenter_cloudprovider_instance_launch_failures_total`.**
  ICE errors fall under `reason="LaunchFailed"`. Use
  `karpenter_cloudprovider_errors_total{error="InsufficientCapacityError"}` or
  `karpenter_nodeclaims_disrupted_total{reason="insufficient_capacity"}` instead. One `CreateFleet`
  call can also fail many offerings at once, so `instance_launch_failures_total` counts offerings,
  not launches.
* **`NodeClaimNotFoundError` in `karpenter_cloudprovider_errors_total` is normal** during
  termination. Don't alert on it.
* **`aws_sdk_go_request_total{code="503"}` undercounts throttling.** It reports the final status of
  each request, so throttles that succeeded on retry disappear. Use `aws_sdk_go_request_attempt_total`.
* **`karpenter_interruption_received_messages_total` counts every message in the queue,** including
  events for instances Karpenter doesn't manage, so it won't match `karpenter_nodeclaims_disrupted_total`.
* **The absence of `karpenter_voluntary_disruption_decisions_total` increments is ambiguous.** It looks
  the same whether Karpenter evaluated nodes and decided to do nothing, or never got to evaluate them.
  Pair it with `karpenter_voluntary_disruption_eligible_nodes` and the disruption workqueue metrics.
* **Prefer the per-kind `operator_<kind>_*` metrics** (for example
  `operator_nodeclaim_status_condition_count`) over the generic `operator_status_condition_count`
  with `kind` labels. The generic metrics are deprecated and don't cover Nodes.

## Failure modes without a direct metric

Some failures have no metric today. Cover them as follows:

| Failure mode | How to detect it |
|---|---|
| Pods force-deleted after `terminationGracePeriod` | Pod `Disrupted` Events ([above](#detecting-forceful-termination-after-terminationgraceperiod)). |
| Disruption blocked by `do-not-disrupt` or PDBs | `DisruptionBlocked` Events; nodes carrying the `karpenter.sh/disrupted` taint for more than an hour; kube-state-metrics `kube_poddisruptionbudget_status_pod_disruptions_allowed == 0` for more than an hour. |
| Upcoming expiration | Compare `karpenter_nodes_current_lifetime_seconds` with each NodePool's `expireAfter` ([above](#disruption-and-termination)). |
| Provisioning silently stalled by an invalid requirement | The [pending-pods backstop alert](#critical-alerts), plus `controller_runtime_reconcile_errors_total{controller="provisioner"}`. |
| Karpenter running against an unsupported Kubernetes version | Log alert on `karpenter is not compatible with kubernetes version`. Check the [compatibility matrix]({{< relref "../upgrading/compatibility" >}}) before every cluster upgrade. |
| Something other than Karpenter rewriting NodePools, NodeClaims, or Karpenter's CRDs (for example a GitOps controller fighting a change) | Amazon EKS control plane audit logs: writes to `nodepools`, `nodeclaims`, `ec2nodeclasses`, or Karpenter's CRDs by users other than Karpenter's service account. |

## Operational practices

* **Watch the controller closely after every upgrade.** Compare CPU, memory, restarts,
  `karpenter_scheduler_scheduling_duration_seconds`, EC2 API call volume, and disruption rates before
  and after. Upgrades have historically changed CPU and memory use and EC2 API call volume. Start sizing at
  1 vCPU and 1 GiB of memory per replica and grow from there, and don't let a VPA raise requests
  beyond what the nodes that run Karpenter can fit.
* **Keep Karpenter's own capacity independent of Karpenter.** Run the controller on a managed node
  group or AWS Fargate, not on nodes it manages.
* **Keep logs, and know how to get more.** Retain controller logs for at least a few weeks, and keep
  a runbook for temporarily switching to `debug` logging during an incident.
* **Build a dashboard per NodePool** with node count, NodeClaims created and disrupted by reason,
  usage against limits, launch failures, pending pods, and allowed disruptions. Most incident
  investigations start with "what changed on this NodePool, and when?"
* **Alert on changes to the inputs, not just the outputs.** A new AMI release, a NodePool or
  EC2NodeClass change, or a cluster upgrade often comes just before a wave of drift or launch
  failures. Annotate your dashboards with these events.

## Example PrometheusRule

The following `PrometheusRule` implements a subset of the critical alerts. Adjust the namespace,
labels, and thresholds for your environment.

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: karpenter
  namespace: kube-system
spec:
  groups:
    - name: karpenter.critical
      rules:
        - alert: KarpenterMetricsMissing
          expr: absent(karpenter_build_info)
          for: 5m
          labels:
            severity: critical
        - alert: KarpenterNoLeader
          expr: |
            max(leader_election_master_status{name="karpenter-leader-election"}) < 1
              or absent(leader_election_master_status{name="karpenter-leader-election"})
          for: 2m
          labels:
            severity: critical
        - alert: KarpenterClusterStateUnsynced
          expr: max(karpenter_cluster_state_unsynced_time_seconds) > 600
          labels:
            severity: critical
        - alert: KarpenterPendingPodsNoNodeClaims
          expr: |
            (max(karpenter_pods_provisioning_unbound_time_seconds) > 600)
              and on() (sum(increase(karpenter_nodeclaims_created_total[15m])) == 0)
          for: 5m
          labels:
            severity: critical
        - alert: KarpenterNodePoolRegistrationUnhealthy
          expr: max by (name) (operator_nodepool_status_condition_count{type="NodeRegistrationHealthy", status="False"}) > 0
          for: 5m
          labels:
            severity: critical
        - alert: KarpenterEC2NodeClassNotReady
          expr: max by (name) (operator_ec2nodeclass_status_condition_count{type="Ready", status="False"}) > 0
          for: 5m
          labels:
            severity: critical
        - alert: KarpenterDisruptionWedged
          expr: max(workqueue_unfinished_work_seconds{name="disruption"}) > 1800
          labels:
            severity: critical
    - name: karpenter.warning
      rules:
        - alert: KarpenterNodeClaimStuckTerminating
          expr: max by (name, karpenter_sh_nodepool) (operator_nodeclaim_termination_current_time_seconds) > 3600
          labels:
            severity: warning
        - alert: KarpenterNodePoolNearLimit
          expr: max by (nodepool, resource_type) (karpenter_nodepools_usage / on(nodepool, resource_type) karpenter_nodepools_limit) > 0.9
          for: 15m
          labels:
            severity: warning
        - alert: KarpenterEC2Throttling
          expr: |
            sum(rate(aws_sdk_go_request_attempt_total{service="EC2", code="503"}[5m]))
              / sum(rate(aws_sdk_go_request_attempt_total{service="EC2"}[5m])) > 0.05
          for: 15m
          labels:
            severity: warning
```
