# v2sudoku

`v2sudoku` is a v2board node backend for the official [SUDOKU-ASCII/sudoku](https://github.com/SUDOKU-ASCII/sudoku) protocol.

It talks to v2board with the same UniProxy interface used by `v2node`:

- node config: `/api/v2/server/config`
- users: `/api/v1/server/UniProxy/user`
- traffic push: `/api/v1/server/UniProxy/push`
- online push: `/api/v1/server/UniProxy/alive`

## Runtime Modes

`Runtime.Engine: embedded` is the recommended mode. It embeds the official Sudoku Go module, handles Sudoku handshakes in-process, maps the client handshake `user_hash` back to v2board users, and reports per-user traffic.

`Runtime.Engine: external` generates `server.config.json` and starts `Runtime.SudokuPath -c <config>`. This is useful for compatibility testing, but stock Sudoku CLI does not emit per-user byte events, so v2sudoku cannot deduct traffic unless you run a patched runtime that emits JSON events.

## v2board Node Fields

Create a `v2node` server with `protocol=sudoku`.

Put Sudoku settings in `encryption_settings`:

```json
{
  "master_public_key": "MASTER_PUBLIC_KEY_HEX",
  "aead": "chacha20-poly1305",
  "ascii": "prefer_entropy",
  "padding_min": 5,
  "padding_max": 15,
  "enable_pure_downlink": true,
  "fallback_address": "127.0.0.1:80",
  "suspicious_action": "fallback",
  "httpmask": {
    "disable": false,
    "mode": "legacy"
  }
}
```

For the recommended `deterministic_split` mode and the v2board subscription patch, also keep:

```json
{
  "master_private_key": "MASTER_PRIVATE_KEY_HEX"
}
```

The subscription patch uses `master_private_key` server-side to derive a per-user split private key; it does not put the master private key itself into subscriptions.

## Client Keys

Sudoku clients need a client `key`.

- `deterministic_split`: derives a real Sudoku split private key from `master_private_key`, node id, user id, and UUID. This is the recommended mode and matches the v2board subscription patch.
- `uuid`: uses the v2board user UUID as the Sudoku client key. This is only useful for non-official raw API clients and tests; official Sudoku tunnel clients expect a valid master/split private key.
- `deterministic`: derives a stable hex key from `ClientKeySeed`, node id, user id, and UUID.
- `auto`: generates real Sudoku split private keys from `master_private_key` and stores them in `ClientKeyFile`.

The official Sudoku server validates by master public key, not by a revocable per-user list. If one user's split key leaks, removing the user from v2board stops accounting/online reporting for that hash, but the stock protocol can only fully revoke access by rotating the master key.

## Troubleshooting

`reject sudoku connection with unknown user hash` means the TCP connection and Sudoku handshake reached the node, but the client key does not match any user key currently loaded from v2board.

Check these first:

```bash
grep -n "ClientKeySource" /etc/v2sudoku/config.yml
```

The recommended v2board integration should use:

```yaml
Runtime:
  ClientKeySource: deterministic_split
```

Then restart and confirm the node built user hash mappings:

```bash
systemctl restart v2sudoku
journalctl -u v2sudoku -n 100 --no-pager | grep -E "rebuilt sudoku user hash mappings|embedded sudoku listener started|unknown user hash"
```

If `users` or `user_hashes` is `0`, v2board did not return that user to the node. Check the user's status, expiry, traffic balance, group access, and that the node id in `/etc/v2sudoku/config.yml` matches the Sudoku node id in v2board.

If mappings exist but the same hash is still unknown, refresh or re-import the client subscription after saving the Sudoku node. A changed `master_private_key`, node id, or user id changes the derived client key.

## Build

```bash
go build -o v2sudoku .
```

## Run

```bash
cp config.yml.example /etc/v2sudoku/config.yml
./v2sudoku -config /etc/v2sudoku/config.yml
```

## Install

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/ssw-cloud/v2sudoku/main/script/install.sh) \
  --api-host https://panel.example.com \
  --node-id 1 \
  --api-key your-token
```

Multiple nodes can be installed in one config:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/ssw-cloud/v2sudoku/main/script/install.sh) \
  --api-host https://panel.example.com \
  --node-id 1,2,3 \
  --api-key your-token
```

The installer first downloads `v2sudoku_linux_<arch>.tar.gz` from GitHub Releases and falls back to building from source.

## v2board Patch

This repository includes the node backend. Your v2board must also allow `protocol=sudoku`, return `encryption_settings` from `/api/v2/server/config`, and generate client subscriptions using the same key source.
