# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [2.0.0] - 2026-11-14

### Added

- Put the time a line was logged at in front of it when Tekton Dashboard asks for it with `timestamps=true`, so that the dashboard shows timestamps for logs from Loki as it does for logs of running pods.

### Changed

- Query Loki and serve requests with the Go standard library only. Without `grafana/loki`, `gofiber/fiber` and all they depend on, the binary shrinks from 190 MB to 11 MB.
- Send a log while it is fetched from Loki, 5000 lines at a time, instead of collecting it first. The memory needed no longer grows with the log.
- Log requests as JSON.
- Refuse to start when `lokiEndpoint` is not an `http` or `https` URL. Before, the proxy started and terminated with the first request.
- Serve logs at `/logs/<namespace>/<pod>/<container>` only, no longer with a trailing slash or in another case as well.

### Fixed

- Respond with `502 Bad Gateway` when a query to Loki fails. If a part of the log is sent already, end it with a line starting with `::error::`, which Tekton Dashboard shows as an error, and abort the response. Before, the proxy terminated.
- Quote the names of namespace, pod and container in the query to Loki. Before, a `"` in one of them, in a request sent to the proxy directly, changed the query or terminated the proxy.
- Return lines with the same timestamp in the order Loki has them, and all of them when they are equal. Before, their order was arbitrary, and one of two equal lines was lost when Loki returned them in different batches.
- Return the log of a container with 5000 or more lines for a single timestamp: the first 5000 of them, and all that follows. Before, the proxy terminated.

## [1.0.0] - 2026-11-02

### Fixed

- The `helm.sh/chart` label is valid for long chart versions: the 63-character cut trims the whole trailing run of `-`, `.` and `_`.
- Revert prometheus/common to v0.44.0 to fix build broken by incompatible renovate bumps.
- Fix Dockerfile `as` to `AS` casing warning.

### Changed

- Update gofiber/fiber to v2.52.12.

## [0.5.0] - 2026-03-03

### Changed

- Migrate to App Build Suite (ABS) for building and publishing Helm charts.
- Chart: Add `io.giantswarm.application.audience: giantswarm` annotation to `Chart.yaml`.
- Chart: Migrate team annotation from `application.giantswarm.io/team` to `io.giantswarm.application.team: tenet`.

## [0.4.1] - 2025-09-02

### Changed

- Load container image from `gsoci.azurecr.io` by default

## [0.4.0] - 2024-09-16

### Changed

- Change `app.giantswarm.io/*` labels to `application.giantswarm.io/`
- Update Golang to v1.23.1

## [0.3.0] - 2024-02-01

### Added

- Added `securityContext` for pod and container

## [0.2.2] - 2023-10-23

### Changed

- Bumped Fiber to latest version

## [0.2.1] - 2023-07-20

### Changed

- Updated to Go v1.20
- Updated gofiber/fiber to v2.48
- Updated prometheus/common to v0.44

## [0.2.0] - 2023-06-12

### Changed

- Stopped using parallel queries as they sometimes resulted in un-ordered log output

## [0.1.0] - 2023-05-16

### Added

- Support for the Tekton Dashboard passing in a start / end time for the logs. (See https://github.com/tektoncd/dashboard/pull/2909)

## [0.0.3] - 2023-04-14

### Added

- Request logging

### Changed

- Recreate the query and client objects for each request

## [0.0.2] - 2023-04-13

### Fixed

- Fix Helm chart

## [0.0.1] - 2023-04-13

### Added

- Initial release
- Helm chart

[Unreleased]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v2.0.0...HEAD
[2.0.0]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v1.0.0...v2.0.0
[1.0.0]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.5.0...v1.0.0
[0.5.0]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.4.1...v0.5.0
[0.4.1]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.2.2...v0.3.0
[0.2.2]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.0.3...v0.1.0
[0.0.3]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.0.2...v0.0.3
[0.0.2]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/releases/tag/v0.0.1
