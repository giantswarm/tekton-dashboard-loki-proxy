# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.2.2...HEAD
[0.2.2]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.0.3...v0.1.0
[0.0.3]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.0.2...v0.0.3
[0.0.2]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/giantswarm/tekton-dashboard-loki-proxy/releases/tag/v0.0.1
