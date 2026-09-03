# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) with 0.x semantics: minor versions may break the API.

## [Unreleased]

## [0.8.4] - 2026-09-02

### Changed

- rio v0.18.1.

## [0.8.3] - 2026-09-02

### Changed

- rio v0.18.0.

## [0.8.2] - 2026-09-02

### Changed

- rio v0.17.0.

## [0.8.1] - 2026-09-02

### Added

- `CONTRIBUTING.md`, `CHANGELOG.md`, `llms.txt`, and compile-only examples for every constructor and `TxOf`.

### Changed

- README restructured; the native channel's types keep their methods together. No API change.

## [0.8.0] - 2026-09-02

### Changed

- rio v0.16.0: preload and `WithCount` key sets bind as one typed array parameter (`= ANY($1)`) on both channels, so the statement text no longer varies with the number of parents.

## [0.7.0] - 2026-08-31

### Added

- The native channel implements rio's batch and copy capabilities: each preload layer runs in one pgx batch round trip, and explicit-key `InsertAll` streams over `COPY`.

### Changed

- rio v0.13.0.

## [0.6.0] - 2026-08-30

### Changed

- The pool handle rides the rio DB: `PoolOf` reads it from `DriverHandle`, on the pgxpool and native paths alike.
- rio v0.12.0.

## [0.5.0] - 2026-08-30

### Fixed

- Native transactions latch completion: a second `Commit` or `Rollback`, or one after the connection died, reports `sql.ErrTxDone` with the pgx error in the chain.

### Changed

- rio v0.11.0.

## [0.4.1] - 2026-08-20

### Changed

- Go 1.27 and PostgreSQL 18 through dependency updates.

## [0.4.0] - 2026-08-09

### Changed

- A dead connection rolls back as `sql.ErrTxDone`.
- rio v0.10.0.

## [0.3.1] - 2026-07-11

### Changed

- rio v0.9.0; release automation; documented arrays and JSONB.

## [0.3.0] - 2026-07-11

### Added

- pgxpool tier: `OpenPool`, `NewFromPool`, `PoolOf`.
- Native pgx tier: `OpenNative`, `NewNativeFromPool`, `TxOf`.

### Changed

- rio v0.8.0.

## [0.2.2] - 2026-07-10

### Changed

- rio v0.7.0.

## [0.2.1] - 2026-07-10

### Changed

- rio v0.6.0.

## [0.2.0] - 2026-07-10

### Added

- `Open` rejects DSNs that turn `standard_conforming_strings` off, including through `options` and `PGOPTIONS`.

### Changed

- rio v0.5.0.

## [0.1.0] - 2026-07-09

### Added

- Initial release: `Open` and `New` over pgx's database/sql adapter, duplicate-key and foreign-key error translation.

[Unreleased]: https://github.com/go-rio/postgres/compare/v0.8.4...HEAD
[0.8.4]: https://github.com/go-rio/postgres/compare/v0.8.3...v0.8.4
[0.8.3]: https://github.com/go-rio/postgres/compare/v0.8.2...v0.8.3
[0.8.2]: https://github.com/go-rio/postgres/compare/v0.8.1...v0.8.2
[0.8.1]: https://github.com/go-rio/postgres/compare/v0.8.0...v0.8.1
[0.8.0]: https://github.com/go-rio/postgres/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/go-rio/postgres/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/go-rio/postgres/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/go-rio/postgres/compare/v0.4.1...v0.5.0
[0.4.1]: https://github.com/go-rio/postgres/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/go-rio/postgres/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/go-rio/postgres/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/go-rio/postgres/compare/v0.2.2...v0.3.0
[0.2.2]: https://github.com/go-rio/postgres/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/go-rio/postgres/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/go-rio/postgres/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/go-rio/postgres/releases/tag/v0.1.0
