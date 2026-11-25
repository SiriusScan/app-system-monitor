# app-system-monitor

The collector takes a resource observation at startup and every five seconds thereafter. It records the observation in `system:metrics:{container_name}` using one Valkey write with a 60-second expiry. Each write has a two-second deadline. After a failed write, the next sample is a new observation with its own timestamp.

`timestamp` is the moment the collector finished gathering the observation. `collector_started_at` identifies the collector process start and `collector_uptime_seconds` is its age at observation time. Existing readers can still decode `status` and `uptime_seconds`, which describe the collector. Sirius probes determine service readiness.

Run the isolated collector fixtures with `go test main.go main_test.go`.
