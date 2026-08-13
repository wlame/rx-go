# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- The compress output path is now validated against the search roots.
  `POST /v1/compress` answers `403` for an `output_path` outside
  `--search-root`, including one reached through a symlink and including
  `force=true`, and `rx compress` refuses `--output` / `--output-dir`
  destinations outside the roots.
- The compress task now reports `compression_ratio` as
  `decompressed / compressed`, the direction its own API documentation and
  both CLIs already use.
