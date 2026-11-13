# performancev2 trends dashboard

`perf-trends.json` is a Grafana dashboard for the E2EPerf nightly results that the Timestream sink
(`../timestream.go`) writes to `karpenterTesting.perfResults` (us-east-2). It needs the
`grafana-timestream-datasource` plugin.

- **Import:** in Grafana, go to Dashboards > New > Import, upload `perf-trends.json`, and pick the us-east-2
  Timestream datasource for `ds`. The uid is `karpenter-perfv2-trends`, so re-importing updates the dashboard in place.
- **Variables:** `ds`, `provider`, `scenario`, `phase` (filtered by scenario), `trigger` (default `schedule`),
  and `metric` (the gated metrics).
- **Rows:**
  - Gates: value against bound, with FAIL and WARN points (under 10% headroom).
  - Latest run: every gate sorted by headroom, linked to its GitHub run.
  - By commit: each metric per `aws_commit`.
  - Context: k8s version, control plane tier, controller shape and core commit over time.

Points are timestamped at the phase's end. If you change the record schema in `timestream.go`, update the
queries here to match.
