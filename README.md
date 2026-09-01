# Starcat History API

<!-- starcat-promo:start -->
<div align="center">
<a href="https://starcat.ink"><img src="https://raw.githubusercontent.com/starcat-app/starcat-pro/main/banner.webp" width="100%" alt="Starcat" /></a>

<p><strong>Starcat's self-hostable API and local data pipeline for estimated public GitHub Star history.</strong></p>
<p>Starcat is a native macOS app that turns GitHub Stars into a searchable, organized and AI-assisted local knowledge base, with a broader ecosystem of desktop clients, plugins, CLI tools, and self-hostable services.</p>

<a href="https://github.com/starcat-app/homebrew-starcat"><img src="https://img.shields.io/badge/Install%20with-Homebrew-FBBF24?style=for-the-badge&logo=homebrew&logoColor=white" width="220" alt="Install with Homebrew"/></a>
<br/>
<sub><a href="./README-ZH.md">中文说明</a></sub>
</div>

<div align="center">
<a href="https://starcat.ink"><img src="https://img.shields.io/badge/website-starcat.ink-38BDF8?style=flat&color=blue" alt="website"/></a>
<a href="https://github.com/starcat-app/starcat-pro"><img src="https://img.shields.io/badge/support-starcat--pro-lightgrey.svg?style=flat&color=blue" alt="support"/></a>
<a href="https://github.com/starcat-app/homebrew-starcat"><img src="https://img.shields.io/badge/install-homebrew-lightgrey.svg?style=flat&color=blue" alt="homebrew"/></a>
<a href="https://github.com/starcat-app/starcat-localization"><img src="https://img.shields.io/badge/localization-open-lightgrey.svg?style=flat&color=blue" alt="localization"/></a>
</div>

<div align="center">
<img width="900" src="https://raw.githubusercontent.com/starcat-app/starcat-pro/main/main.webp" alt="Starcat main window"/>
</div>

**Preferred install method:**

```bash
brew tap starcat-app/starcat
brew trust starcat-app/starcat
brew install --cask starcat
```

**Useful links:**

- Home and downloads: https://starcat.ink
- Mac App Store: search for Starcat for GitHub
- Public support and release notes: https://github.com/starcat-app/starcat-pro
- CLI / MCP: [starcat-cli](https://github.com/starcat-app/starcat-cli) / [Homebrew tap](https://github.com/starcat-app/homebrew-starcat-cli)
- AI Agent Skill: https://github.com/starcat-app/starcat-skill
- Browser plugins: [Chrome](https://github.com/starcat-app/starcat-chrome-plugin) / [Safari](https://github.com/starcat-app/starcat-safari-plugin)
- Documentation: https://github.com/starcat-app/starcat-docs
- Website source: https://github.com/starcat-app/starcat-site
- Localization: https://github.com/starcat-app/starcat-localization

**Self-hostable support APIs:**

- [starcat-sharing-api](https://github.com/starcat-app/starcat-sharing-api)
- [starcat-trending-api](https://github.com/starcat-app/starcat-trending-api)
- [starcat-weekly-api](https://github.com/starcat-app/starcat-weekly-api)
- [starcat-wiki-api](https://github.com/starcat-app/starcat-wiki-api)
- [starcat-recommend-api](https://github.com/starcat-app/starcat-recommend-api)
- [starcat-discovery-api](https://github.com/starcat-app/starcat-discovery-api)
- [starcat-history-api](https://github.com/starcat-app/starcat-history-api)

> Starcat provides hosted defaults for normal users. This API is designed so advanced users can inspect it, run it locally, or deploy their own instance after the repository's public-release review is complete.
<!-- starcat-promo:end -->

`starcat-history-api` builds and serves estimated Star-history curves for public GitHub repositories. It contains two deliberately separated components:

- **Local Builder** reads GH Archive `WatchEvent` Parquet files and produces auditable daily Silver datasets, immutable full snapshots, and adjacent daily deltas.
- **Serving API** validates and activates published snapshots or deltas, calibrates event curves against the current public GitHub `stargazers_count`, and exposes stable REST endpoints to Starcat and self-hosted clients.

Raw GH Archive events remain on the local data platform. Published serving bundles contain only compressed `repo_id + event_day + event_count` series. They do not contain GitHub actors, event payloads, Starcat users, credentials, private repositories, notes, tags, searches, or AI/RAG content.

For the full Chinese operations guide, see [README-ZH.md](./README-ZH.md). Production daily orchestration is documented in the Starcat repository's [WatchEvent and Star History daily incremental operations guide](https://github.com/starcat-app/Starcat/blob/main/docs/2-产品/需求讨论/推荐算法/WatchEvent与Star-History每日增量运维指南.md).

## Data flow

```text
GH Archive WatchEvent Raw Parquet
  -> History Silver (repo_id + event_day + event_count)
  -> Snapshot / Daily Delta ZIP
  -> starcat-history-api Registry
  -> GET /api/v1/repos/{owner}/{repo}/star-history
  -> Starcat repository insights
```

GH Archive does not reliably express Unstar events. The public curve is therefore an estimate:

```text
estimatedStars(day) = round(currentStars * cumulativeEvents(day) / totalEvents)
```

Every public point is marked with `source=gh_archive` and `precision=estimated`. The final covered point is fixed to the repository's current public Star count.

## Requirements

- Go 1.25+
- Python 3.11+
- [uv](https://docs.astral.sh/uv/)
- Docker, only when validating or building the container image
- A data volume with enough temporary space for full Builder jobs; large jobs should not use the system disk's default temporary directory

## Quality gates

Run all Go and Builder checks:

```bash
make test
```

Equivalent commands:

```bash
go test ./...
go vet ./...

cd builder
uv sync --frozen --extra test --python 3.12
uv run pytest -q
```

Build the API binary or container:

```bash
make build
docker build -t starcat-history-api:local .
```

## Run the API locally

```bash
cp .env.example .env
# Set separate API_KEYS and PUBLISH_KEYS values in .env.
make run
```

The default listener is `http://127.0.0.1:5014`.

```bash
curl -fsS http://127.0.0.1:5014/healthz

curl -fsS \
  -H 'Authorization: Bearer local-history-client-key' \
  http://127.0.0.1:5014/api/v1/ping
```

Do not commit `.env`, API keys, publish keys, GitHub tokens, generated SQLite databases, Raw Parquet files, snapshots, deltas, or publish receipts.

## Builder artifacts

The Builder has three production artifact types:

| Artifact | Input | Purpose |
|---|---|---|
| Silver | GH Archive Raw WatchEvent Parquet | Auditable `repo_id + event_day + event_count` aggregation |
| Snapshot | Silver or compatible canonical input | Complete immutable serving baseline |
| Delta | One completed UTC day | Adjacent, idempotent daily update |

Build a Silver dataset:

```bash
cd builder

uv run starcat-history-builder silver \
  --input '/path/to/raw/watch-events-*.parquet' \
  --output-dir /path/to/history/silver \
  --temp-dir /path/to/history/tmp \
  --dataset-id watch-silver-20260825-v1 \
  --watermark 2026-08-25 \
  --memory-limit 12GB \
  --threads 6
```

Build an immutable snapshot from Silver:

```bash
uv run starcat-history-builder snapshot \
  --input '/path/to/history/silver/watch-silver-20260825-v1/data/**/*.parquet' \
  --output-dir /path/to/history/snapshots \
  --temp-dir /path/to/history/tmp \
  --model-version watch-history-20260825-v1 \
  --watermark 2026-08-25 \
  --memory-limit 12GB \
  --threads 6
```

The snapshot directory contains:

```text
watch-history-20260825-v1/
├── history.sqlite
├── manifest.json
├── checksums.json
└── watch-history-20260825-v1.zip
```

Build an adjacent daily delta:

```bash
uv run starcat-history-builder delta \
  --input /path/to/raw/watch-events-20260826.parquet \
  --output-dir /path/to/history/deltas \
  --temp-dir /path/to/history/tmp \
  --delta-id watch-delta-20260826-v1 \
  --from-watermark 2026-08-25 \
  --to-watermark 2026-08-26 \
  --memory-limit 4GB \
  --threads 4
```

The server rejects date gaps. Snapshot and delta publication advance `active_watermark` only after checksum, manifest, SQLite schema, source continuity, and transactional-apply validation all succeed.

## Install a local snapshot

Stop the process using port `5014`, then install a Builder snapshot into the configured `STORE_FILE`:

```bash
make install-local-snapshot \
  SNAPSHOT=/path/to/watch-history-20260825-v1

# Explicitly replace an existing local store.
make install-local-snapshot \
  SNAPSHOT=/path/to/watch-history-20260825-v1 \
  FORCE=1
```

This path is intended for local query validation. Production and incremental operation should use the publish API and immutable Registry instead.

## Publish snapshots and deltas

The standalone service does not need a gateway header:

```bash
export HISTORY_API_BASE_URL=http://127.0.0.1:5014
export HISTORY_PUBLISH_KEY=local-history-publish-key

scripts/publish-bundle.sh snapshot \
  watch-history-20260825-v1 \
  /path/to/watch-history-20260825-v1.zip \
  true

scripts/publish-bundle.sh delta \
  watch-delta-20260826-v1 \
  /path/to/watch-delta-20260826-v1.zip
```

When publishing through the Starcat aggregate gateway, set `HISTORY_GATEWAY_SERVICE=history`. Production keys belong in the deployment secret store or the local Keychain-backed orchestrator, never in shell history or committed environment files.

Snapshot activation keeps the previous active version available until the new version passes validation. Applying a delta, recording its idempotency receipt, merging affected compressed series, and advancing `active_watermark` happen in one transaction. A failed publication continues serving the previous active version.

## API

| Method | Path | Authentication | Purpose |
|---|---|---|---|
| `GET` | `/healthz` | None | Process health check |
| `GET` | `/api/v1/ping` | `API_KEYS` | Client connectivity probe |
| `GET` | `/api/v1/repos/{owner}/{repo}/star-history` | `API_KEYS` | Calibrated public-repository curve |
| `GET` | `/api/v1/repos/{owner}/{repo}/star-history/events` | `API_KEYS` | Raw daily event counts for Starcat-side calibration |
| `GET` | `/internal/stats` | `API_KEYS` | Constant-time serving scale and watermark statistics |
| `GET` | `/internal/metrics/*` | `API_KEYS` | Aggregated service metrics |
| `POST` | `/internal/v1/history-snapshots/{version}?activate=true` | `PUBLISH_KEYS` | Validate, install, and optionally activate a snapshot |
| `POST` | `/internal/v1/history-snapshots/{version}/activate` | `PUBLISH_KEYS` | Switch to an installed snapshot |
| `POST` | `/internal/v1/history-deltas/{delta_id}` | `PUBLISH_KEYS` | Idempotently apply an adjacent daily delta |
| `GET` | `/internal/v1/history-active` | `PUBLISH_KEYS` | Read the active version and watermark |

Query the calibrated compatibility endpoint:

```bash
curl -fsS \
  -H 'Authorization: Bearer local-history-client-key' \
  'http://127.0.0.1:5014/api/v1/repos/vinta/awesome-python/star-history?repo_id=21289110&range=all&current_stars=120000'
```

Query the event endpoint used by Starcat:

```bash
curl -fsS \
  -H 'Authorization: Bearer local-history-client-key' \
  'http://127.0.0.1:5014/api/v1/repos/vinta/awesome-python/star-history/events?repo_id=21289110'
```

The compatibility endpoint supports `range=3m|1y|all`, `ETag`, and `If-None-Match`. Supplying a valid non-negative `current_stars` avoids a GitHub metadata request. The event endpoint never requests GitHub metadata; its `events[].count` values are daily WatchEvent counts, not cumulative Star counts.

## Security and privacy boundary

- Client query keys and internal publish keys are separate trust domains.
- Uploads accept only the documented ZIP allowlist and validate streaming SHA-256, manifest contents, SQLite schema, statistics, and watermarks.
- The service never connects to BigQuery or a private home network and never reads the local Raw data lake.
- Published data excludes actors, event payloads, Starcat identities, credentials, and private or internal repository data.
- GitHub tokens, when configured, are used only to read current public repository metadata.
- Vulnerabilities should be reported privately through GitHub Security Advisories, as described in [SECURITY.md](./SECURITY.md).

## Deployment boundary

Starcat production runs History as a module behind `starcat-api`, selected with `X-SC-Svc: history`; it does not require a dedicated Fly app. The standalone binary and Dockerfile remain supported for local validation and self-hosted deployments.

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md), [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md), and [SUPPORT.md](./SUPPORT.md). Keep API, Builder, privacy, and publication contracts aligned, and include focused tests for behavior changes.

## License

[MIT](./LICENSE)
