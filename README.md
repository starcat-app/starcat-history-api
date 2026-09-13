# Starcat History API

<!-- starcat-promo:start -->
<div align="center">
<a href="https://starcat.ink"><img src="https://raw.githubusercontent.com/starcat-app/starcat-pro/main/banner.webp" width="100%" alt="Starcat" /></a>

<p><strong>Starcat's self-hostable API and cache-first public GitHub Star history service.</strong></p>
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

`starcat-history-api` is a public GitHub Star History service for self-hosted deployments. It does one thing: given a public repository name, it returns that repository's star history — as a calibrated JSON curve, or as a self-contained SVG card ready for README embedding.

## Core features

### 1. Star history API

`GET /api/v1/repos/{owner}/{repo}/star-history` returns the cumulative star curve of a public repository. Data comes from GitHub's official `stargazers/history` API, rebuilt into daily cumulative points and calibrated against the current public `stargazers_count`. A two-level cache (in-memory LRU plus SQLite) with ETag incremental refresh keeps high-volume requests from repeatedly hitting GitHub.

### 2. Embed star history in a public README

`GET /embed/v1/repos/{owner}/{repo}/star-history.svg` renders the same history as a self-contained SVG card. It requires no API key, no JavaScript, no remote styles, and no remote images, so it works directly inside GitHub README rendering, with light/dark theme and English/Chinese locale options.

## Deprecation notice

The legacy GH Archive WatchEvent data path is being retired and will be removed in an upcoming release:

- `GET /api/v1/repos/{owner}/{repo}/star-history/events` — raw daily WatchEvent counts;
- `POST /internal/v1/history-snapshots/*`, `POST /internal/v1/history-deltas/*`, `GET /internal/v1/history-active` — Builder publish endpoints;
- The Builder itself, its Silver/Snapshot/Delta artifacts, and the local WatchEvent daily-sync orchestration.

All public curve and SVG embed responses already come from GitHub's official API and are unaffected. New integrations should not use the endpoints above. For background, rationale, and the full retirement plan, see [官方API切换与WatchEvent链路下线方案](./docs/官方API切换与WatchEvent链路下线方案.md).

## Data flow

```text
GitHub /repos/{owner}/{repo}/stargazers/history
  -> SQLite weekly payload cache + in-memory LRU
  -> daily cumulative curve + current-Star calibration
  -> GET /api/v1/repos/{owner}/{repo}/star-history
  -> Starcat repository insights / SVG Embed
```

GitHub returns weekly `week`, `total`, and seven daily increment values. Since the API does not provide historical Unstar events, the service uses the current public Star count as the final calibration anchor:

```text
estimatedStars(day) = round(currentStars * cumulativeEvents(day) / totalEvents)
```

Every public point is marked with `source=github_history` and `precision=reconstructed`. A cold repository fetch paginates the official endpoint, SQLite history remains fresh for 24 hours, and expired data uses an ETag incremental refresh; a full validation is performed after seven days.

## Requirements

- Go 1.25+
- Docker, only when validating or building the container image

`GITHUB_TOKENS` (comma-separated pool, round shared across requests) or `GITHUB_TOKEN` (single) is optional; without one the service runs under GitHub's anonymous rate limits.

## Quality gates

```bash
make test
```

Equivalent commands:

```bash
go test ./...
go vet ./...
```

## Run the API locally

```bash
cp .env.example .env
# At minimum, set API_KEYS in .env.
make run
```

The default listener is `http://127.0.0.1:5014`.

The in-process cache for official Star history defaults to 30 minutes and can be tuned with
`OFFICIAL_MEMORY_CACHE_TTL_SECONDS` in `.env`. This only changes the memory layer; the SQLite
official-history cache remains valid for 24 hours.

```bash
curl -fsS http://127.0.0.1:5014/healthz

curl -fsS \
  -H 'Authorization: Bearer local-history-client-key' \
  http://127.0.0.1:5014/api/v1/ping
```

Do not commit `.env`, API keys, GitHub tokens, or generated SQLite databases.

## API

| Method | Path | Authentication | Purpose |
|---|---|---|---|
| `GET` | `/healthz` | None | Process health check |
| `GET` | `/api/v1/ping` | `API_KEYS` | Client connectivity probe |
| `GET` | `/embed/v1/repos/{owner}/{repo}/star-history.svg` | None | Public self-contained SVG for README embedding |
| `GET` | `/api/v1/repos/{owner}/{repo}/star-history` | None | Calibrated public-repository curve |
| `GET` | `/internal/stats` | `API_KEYS` | Serving scale and cache statistics |
| `GET` | `/internal/metrics/*` | `API_KEYS` | Aggregated service metrics |

Query the calibrated curve:

```bash
curl -fsS \
  'http://127.0.0.1:5014/api/v1/repos/vinta/awesome-python/star-history?repo_id=21289110&range=all&current_stars=120000'
```

The curve endpoint supports `range=3m|1y|all`, `ETag`, and `If-None-Match`. `repo_id` is optional; supplying a valid non-negative `current_stars` also avoids a GitHub metadata request.

### Embed Star History in a public README

The public SVG endpoint does not require an API key. Copy the following HTML into a public repository README (do not wrap it in a code block), then replace `OWNER` and `REPO`:

```html
<a href="https://github.com/starcat-app/Starcat" target="_blank" rel="noopener noreferrer">
  <picture data-starcat-star-history>
    <source
      media="(prefers-color-scheme: dark)"
      srcset="https://history.starcat.ink/embed/v1/repos/OWNER/REPO/star-history.svg?theme=dark&amp;locale=en">
    <img
      alt="OWNER/REPO Star History"
      src="https://history.starcat.ink/embed/v1/repos/OWNER/REPO/star-history.svg?theme=light&amp;locale=en">
  </picture>
</a>
```

For example, the card below:
```
<a href="https://github.com/starcat-app/Starcat" target="_blank" rel="noopener noreferrer">
  <picture data-starcat-star-history>
    <source
      media="(prefers-color-scheme: dark)"
      srcset="https://history.starcat.ink/embed/v1/repos/starcat-app/Starcat/star-history.svg?theme=dark&amp;locale=en">
    <img
      alt="starcat-app/Starcat Star History"
      src="https://history.starcat.ink/embed/v1/repos/starcat-app/Starcat/star-history.svg?theme=light&amp;locale=en">
  </picture>
</a>
```

<a href="https://github.com/starcat-app/Starcat" target="_blank" rel="noopener noreferrer">
  <picture data-starcat-star-history>
    <source
      media="(prefers-color-scheme: dark)"
      srcset="https://history.starcat.ink/embed/v1/repos/starcat-app/Starcat/star-history.svg?theme=dark&amp;locale=en">
    <img
      alt="starcat-app/Starcat Star History"
      src="https://history.starcat.ink/embed/v1/repos/starcat-app/Starcat/star-history.svg?theme=light&amp;locale=en">
  </picture>
</a>

The endpoint accepts only `theme=light|dark` and `locale=en|zh`, verifies that the repository is public, and returns a cacheable SVG without JavaScript, remote styles, or remote images. A repository must have at least two history points from the official endpoint before an image is available.

Use `&amp;` for query separators inside HTML attributes and plain `&` in shell commands. To download the SVG directly:

```bash
curl -fsS \
  'https://history.starcat.ink/embed/v1/repos/OWNER/REPO/star-history.svg?theme=light&locale=en' \
  -o star-history.svg
```

For local testing, replace the host with `http://127.0.0.1:5014`. A localhost URL is accessible only from the current machine and cannot be used directly by GitHub README rendering.

## Security and privacy boundary

- The service never stores GitHub identities, actors, event payloads, Starcat user data, or private/internal repository data.
- GitHub tokens, when configured, are used to read current public repository metadata and the official public Star history endpoint.
- Vulnerabilities should be reported privately through GitHub Security Advisories, as described in [SECURITY.md](./SECURITY.md).

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md), [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md), and [SUPPORT.md](./SUPPORT.md). Keep API and privacy contracts aligned, and include focused tests for behavior changes.

## License

[MIT](./LICENSE)
