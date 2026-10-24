# nixhook

Deploy NixOS flake configurations over HTTP, and watch the deploy until it succeeds or fails.

Each box runs `nixhook serve` on localhost, usually exposed through a Cloudflare tunnel. A deploy request names a flake tag (`nixosConfigurations.<tag>`). The box syncs its flake, builds, switches, checks that no services broke, and streams progress back to `nixhook deploy`.

```
$ nixhook deploy --box homeServer
→ job 20261023T153012Z-ab12cd started on https://deploy-home.example.com (target homeServer, ref main)
==> [sync]
...
[sync]    ✓ (1s)
[build]   ✓ (3m12s)
  connection lost (stream closed), reconnecting…
  reconnected
[switch]  ✓ (14s)
[verify]  ✓ (11s)
✔ SUCCESS gen 211 → 212, rev a1b2c3d → e4f5g6h in 3m38s
```

`nixhook deploy` exits `0` on success, `1` if the deploy failed, and `2` on usage or connection errors.

## How a deploy runs

1. **sync**: for a local checkout (`/etc/nixos`), runs `git pull --ff-only origin <ref>`. For a flake URI (`github:you/nixos-config`), resolves `<ref>` to a commit, and the later stages are pinned to that commit.
2. **build**: checks the tag exists in `nixosConfigurations`, then runs `nix build` on its toplevel. Most failures happen here, and nothing on the box has changed yet. `--dry-run` stops after this stage.
3. **switch**: runs `nixos-rebuild switch --flake <flake>#<tag>`.
4. **verify**: waits for systemd to settle, then fails if units are failed that weren't failed before the switch. With `rollbackOnFailure`, a failed switch or verify runs `nixos-rebuild switch --rollback`.

Each deploy runs in a transient systemd unit (`nixhook-job`), not inside the server. A switch that restarts nixhook or the tunnel doesn't interrupt it; clients just reconnect. systemd only allows one unit with that name at a time, so a box runs one deploy at a time; a second request gets `409`.

## Install (NixOS)

```nix
# flake.nix
inputs.nixhook.url = "github:canavan-a/nixhook";

# in the host's modules
nixhook.nixosModules.default
{
  services.nixhook = {
    enable = true;
    flake = "/etc/nixos";               # or "github:you/nixos-config"
    defaultTarget = "homeServer";       # used when a request names no target
    allowedTargets = [ "homeServer" ];  # optional; empty = any tag in the flake
    rollbackOnFailure = true;
  };
}
```

On first start the server generates an API key at `/var/lib/nixhook/key`:

```sh
sudo nixhook key show     # copy this to your client
sudo nixhook key rotate   # replace it; takes effect immediately
```

For a local checkout, root needs to be able to `git pull` it (for example with a deploy key in `/root/.ssh`).

### Cloudflare tunnel

The server listens on `127.0.0.1:9099`. Add an ingress rule to your tunnel:

```yaml
- hostname: deploy-home.example.com
  service: http://127.0.0.1:9099
```

Optionally, put a Cloudflare Access application with a service token in front of it, and add the token headers to the box config below.

## Client

`~/.config/nixhook/boxes.json`:

```json
{
  "boxes": {
    "homeServer": {
      "url": "https://deploy-home.example.com",
      "keyFile": "~/.config/nixhook/keys/homeServer",
      "target": "homeServer",
      "headers": {
        "CF-Access-Client-Id": "…",
        "CF-Access-Client-Secret": "…"
      }
    }
  }
}
```

```sh
nixhook deploy  --box homeServer [--target T] [--ref R] [--dry-run] [--no-wait] [--quiet]
nixhook status  --box homeServer
nixhook targets --box homeServer
nixhook jobs    --box homeServer
nixhook logs    --box homeServer [-f] [id|latest]
```

If there is only one box in the config, `--box` can be left out. Without a config file, use `--url` and `--key-file`, or `NIXHOOK_URL` and `NIXHOOK_KEY` / `NIXHOOK_KEY_FILE`.

Ctrl-C only stops watching; the deploy keeps running. Use `nixhook logs -f` to reattach.

## HTTP API

Every `/v1` route requires `Authorization: Bearer <key>`.

| Method | Path | |
|---|---|---|
| GET | `/healthz` | No auth |
| GET | `/v1/status` | Host, generation, rev, failed units, active job |
| GET | `/v1/targets` | Deployable flake tags |
| POST | `/v1/deploy` | `{"target":"homeServer","ref":"main","dryRun":false}` → `202 {"jobId":…}` |
| GET | `/v1/jobs` | Recent jobs |
| GET | `/v1/jobs/{id}` | One job; `latest` works as the ID |
| GET | `/v1/jobs/{id}/logs?follow=1&offset=N` | Server-sent events: `log` (one line, `id` = byte offset), `state` (job JSON), `done` |

```sh
ID=$(curl -s -H "Authorization: Bearer $KEY" -d '{"target":"homeServer"}' \
  https://deploy-home.example.com/v1/deploy | jq -r .jobId)
curl -N -H "Authorization: Bearer $KEY" \
  "https://deploy-home.example.com/v1/jobs/$ID/logs?follow=1"
```

## Development

```sh
nix develop
go test ./...
nix build
```
