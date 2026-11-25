# app-system-monitor

The collector takes a resource observation at startup and every five seconds thereafter. It records the observation in `system:metrics:{container_name}` using one Valkey write with a 60-second expiry. Each write has a two-second deadline. After a failed write, the next sample is a new observation with its own timestamp.

`timestamp` is the moment the collector finished gathering the observation. `collector_started_at` identifies the collector process start and `collector_uptime_seconds` is its age at observation time. Existing readers can still decode `status` and `uptime_seconds`, which describe the collector. Sirius probes determine service readiness.

Each `observation_version: 2` sample uses a number for a measured field, including
a measured zero, and JSON `null` when the measurement is unavailable. Earlier
samples lack this version marker and their resource numbers may include estimates
or failure defaults.

- CPU percent is the cgroup CPU counter delta divided by elapsed time and the
  available CPU capacity. A first sample, counter reset or changed quota starts
  a new baseline before a utilization number can be reported.
- Memory usage comes from `memory.current`. The collector reports
  `memory_limit_bytes` and calculates a percentage only with a valid finite
  `memory.max`. An unlimited cgroup still reports measured usage.
- Receive and transmit byte counters are independent. Process count covers
  processes visible in the collector's `/proc`.
- Disk capacity, usage and I/O, load averages, and file descriptors are
  unavailable. The existing sources supplied estimates, host-wide values or
  collector-only data for these fields.

Run the isolated collector fixtures with `go test -race main.go main_test.go`.
