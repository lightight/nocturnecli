# Deploy Nocturne CLI

This hosts the docs site, one-line installers, optional binary downloads, and
the `/remote` relay at `https://nocturnecli.lol`.

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
./nocturne serve --addr :8080 --bin ./dist
```

`--bin` is optional. If it is present, `/install.sh` and `/install.ps1` first
try to download from `/bin/` on the same host. If a binary is missing, the
installers fall back to GitHub Releases and then `go install`.

## Reverse Proxy

Put the server behind HTTPS. The browser remote client uses Web Crypto, which
requires a secure origin outside localhost.

Caddy example:

```caddyfile
nocturnecli.lol {
	reverse_proxy 127.0.0.1:8080
}
```

Nginx example:

```nginx
server {
    server_name nocturnecli.lol;

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

The CLI default relay is `https://nocturnecli.lol`. During local testing or
self-hosting, point the CLI elsewhere:

```sh
NOCTURNE_RELAY=http://localhost:8080 nocturne
```

Then run `/remote` in the TUI. The relay only forwards ciphertext between the
terminal and browser. The pairing code is used locally on both devices to derive
the AES-GCM key; it is never sent to the server.

## Smoke Test

```sh
curl -fsSL https://nocturnecli.lol/ >/tmp/nocturne.html
curl -fsSL https://nocturnecli.lol/install.sh | head
curl -fsSL https://nocturnecli.lol/install.ps1 | head
curl -fsSL https://nocturnecli.lol/bin/nocturne_linux_amd64 -o /tmp/nocturne
```

For a local relay check:

```sh
NOCTURNE_RELAY=http://localhost:8080 nocturne
```

Run `/remote`, open the printed `/r/<id>` URL, and enter the code shown in the
terminal.
