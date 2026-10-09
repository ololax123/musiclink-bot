# musiclink-bot — Apple Music ↔ Spotify for Signal

A tiny (~7 MB) Docker container, written in Go, that listens to your signal-cli container. When
someone posts an Apple Music link, it replies with the Spotify link. When
someone posts a Spotify link, it replies with the Apple Music link. Replies go
to the same chat and quote the original message.

## Quick start

1. Edit `docker-compose.yml`:
   - `SIGNAL_URL` → your signal container's name and port
   - `SIGNAL_NUMBER` → the number your signal-cli is registered as
   - `networks.signal.name` → the Docker network your signal container is on
     (`docker network ls`)
2. Set `image:` to `ghcr.io/<your-github-user>/musiclink-bot:latest`
   (or swap to `build: .` to build locally), then:
   ```sh
   docker compose up -d
   docker logs -f musiclink-bot
   ```

If your signal container is defined in the same compose file, drop the
`networks:` block and just put both services in one file.

## GitHub build pipeline

`.github/workflows/docker.yml` runs on every push and pull request:

1. **test** — `gofmt`, `go vet`, and `go test -race`: unit tests incl. the
   loop-protection cases, plus end-to-end tests against a fake
   signal-cli-rest-api (websocket) and a fake `signal-cli daemon --tcp`
2. **build** — static binary on a `scratch` image (just the binary + CA certs,
   runs as non-root), for amd64, arm64 and arm/v7 (Raspberry Pi), pushed to
   GitHub Container Registry. Pull requests build but don't push.

Tags: `latest` (main branch), `sha-abc1234` (every commit), and `1.2.3` / `1.2`
when you push a git tag like `v1.2.3`.

Setup:
```sh
git init && git add . && git commit -m "musiclink-bot"
git remote add origin git@github.com:<you>/musiclink-bot.git
git push -u origin main
```
No secrets needed — it uses the built-in `GITHUB_TOKEN`. After the first run,
the package is private by default: either make it public (repo → Packages →
musiclink-bot → Package settings → Change visibility) or log in on your
server with `docker login ghcr.io` using a token with `read:packages`.

To auto-update the running container when a new image lands, add
[Watchtower](https://containrrr.dev/watchtower/) or run
`docker compose pull && docker compose up -d`.

## Local development

```sh
go test -race ./...
go build -o musiclink-bot . && SIGNAL_NUMBER=+358... SIGNAL_URL=http://localhost:8080 ./musiclink-bot
```

## Which signal-cli container do you have?

| Your container | Settings |
|---|---|
| **bbernhard/signal-cli-rest-api**, `MODE=json-rpc` (recommended) | `SIGNAL_BACKEND=rest`, `REST_RECEIVE=ws` |
| bbernhard/signal-cli-rest-api, `MODE=normal` or `native` | `SIGNAL_BACKEND=rest`, `REST_RECEIVE=poll` |
| Plain signal-cli running `daemon --tcp 0.0.0.0:7583` | `SIGNAL_BACKEND=jsonrpc`, `SIGNAL_HOST=<container>`, `SIGNAL_PORT=7583` |

⚠️ With `poll`, the bot *consumes* incoming messages. If something else (e.g.
Home Assistant's Signal integration) also reads from the same container, use
`MODE=json-rpc` + `ws`, where every websocket listener gets every message.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `DIRECTIONS` | `both` | `both`, `am2sp` (Apple→Spotify only) or `sp2am` |
| `ALLOWED_CHATS` | *(all)* | Comma-separated group IDs and/or numbers (numbers apply to 1:1 chats). Group IDs can be the internal ID or the REST API's `group.xxx` form |
| `TRANSLATE_OWN` | `1` | Also translate links you send yourself (when the bot runs on your own number) |
| `COUNTRY` | `FI` | Store/market for lookups |
| `REPLY_AS_QUOTE` | `1` | Quote the original message |
| `REPLY_NOT_FOUND` | `0` | Reply "no match found" when a lookup fails |
| `LOOP_TTL` | `3600` | Seconds the bot remembers links it posted |
| `SPOTIFY_CLIENT_ID` / `SPOTIFY_CLIENT_SECRET` | — | Optional fallback lookup (free app at developer.spotify.com) |
| `LOG_LEVEL` | `INFO` | `DEBUG` for more detail |
| `SIGNAL_HOST` / `SIGNAL_PORT` | `signal-cli` / `7583` | Only for the `jsonrpc` backend |

List group IDs (REST API): `curl http://<signal>:8080/v1/groups/+358XXXXXXXXX`

## How it avoids loops

The bot never translates its own replies:

1. Every reply starts with an invisible marker and a fixed prefix
   (`🎧 Spotify:` / `🍎 Apple Music:`). Messages with either are skipped.
2. Every link the bot posts is remembered for an hour, so if someone copies
   just the link back into the chat, it's ignored.
3. A message that already has both an Apple Music and a Spotify link is left
   alone.

## Lookups

Uses the free song.link (Odesli) API, ~10 requests/min without a key; the bot
retries on rate limits and caches results. `spotify.link` short links are
resolved first. Apple Music playlists have no 1:1 equivalent and are skipped.
