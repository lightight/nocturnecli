# Deploy Nocturne CLI

This hosts the docs site, one-line installers, optional binary downloads, and
the `/remote` relay at `https://nocturnecode.lol`.

## Build

```sh
make dist
```

This creates:

```text
dist/nocturne_darwin_amd64
dist/nocturne_darwin_arm64
dist/nocturne_linux_amd64
dist/nocturne_linux_arm64
dist/nocturne_windows_amd64.exe
dist/nocturne_windows_arm64.exe
```

## Run

```sh
./nocturne serve --addr :8080 --bin ./dist --update-version v0.4.0
```

`--bin` is optional. If it is present, `/install.sh` and `/install.ps1` first
try to download from `/bin/` on the same host. If a binary is missing, the
installers fall back to GitHub Releases and then `go install`.

`/update.json` and `/version` advertise the downloadable CLI version. Set it
with `--update-version`, `NOCTURNE_UPDATE_VERSION`, or a `VERSION` file in the
`--bin` directory. If none is provided, the server falls back to its own compiled
version.

## Anonymous problem reports

```sh
./nocturne serve --addr :8080 --bin ./dist --reports ./reports
```

`--reports <dir>` (or `NOCTURNE_REPORTS_DIR`) enables `POST /api/report`, which
stores sealed, anonymous debug reports that users explicitly send with
`/report send`. Reports are end-to-end-encrypted to the team key baked into
the CLI — the server only stores blobs it cannot read. Fetch them off the box
and open them locally:

```sh
NOCTURNE_REPORT_PRIVKEY=… nocturne report-decrypt reports/<file>.json
```

The private key (`nocturne report-keygen`) must NEVER live on the server.
Clients post to `{BaseURL}/api/report` (default `https://nocturne.lol`), so
route that path to this service in the reverse proxy.

## Reverse Proxy

Put the server behind HTTPS. The browser remote client uses Web Crypto, which
requires a secure origin outside localhost.

Caddy example:

```caddyfile
nocturnecode.lol {
	reverse_proxy 127.0.0.1:8080
}
```

Nginx example:

```nginx
server {
    server_name nocturnecode.lol;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_buffering off;
    }
}
```

## Remote Relay

The CLI default relay is `https://nocturnecode.lol`. During local testing or
self-hosting, point the CLI elsewhere:

```sh
NOCTURNE_RELAY=http://localhost:8080 nocturne
```

Then run `/remote` in the TUI. The relay only forwards ciphertext between the
terminal and browser. The pairing code is used locally on both devices to derive
the AES-GCM key; it is never sent to the server.

## Smoke Test

```sh
curl -fsSL https://nocturnecode.lol/ >/tmp/nocturne.html
curl -fsSL https://nocturnecode.lol/install.sh | head
curl -fsSL https://nocturnecode.lol/install.ps1 | head
curl -fsSL https://nocturnecode.lol/bin/nocturne_linux_amd64 -o /tmp/nocturne
```

For a local relay check:

```sh
NOCTURNE_RELAY=http://localhost:8080 nocturne
```

Run `/remote`, open the printed `/r/<id>` URL, and enter the code shown in the
terminal.
