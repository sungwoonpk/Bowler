# Setup

Running Bowler and the settings it takes.

## Run the service

```bash
export BOWLER_API_KEY=$(openssl rand -hex 24)
go run ./cmd/bowler serve            # listens on 127.0.0.1:8080
```

Or with Docker:

```bash
export BOWLER_API_KEY=$(openssl rand -hex 24)
docker compose up --build
```

To send to a server on your own machine, start it with `--allow-private --allow-http`. Those flags are for development only. They switch off the protection against calling private addresses, so do not use them in production.

To send to receivers on your own network in production, name the range and leave the protection on for everything else: `--allow-cidr 10.20.0.0/16`. See [Private receivers](#private-receivers).

## Configuration

Every setting is a flag and an environment variable, except the three keys, which are environment only so that they do not show in the process list. A flag beats the variable, which beats the default.

| Flag                | Environment              | Default            | Meaning                                                                                       |
| ------------------- | ------------------------ | ------------------ | --------------------------------------------------------------------------------------------- |
| _(none)_            | `BOWLER_API_KEY`         | required           | At least 24 characters. Environment only: a flag would show in the process list               |
| _(none)_            | `BOWLER_ENCRYPTION_KEY`  | off                | 32 bytes as base64 (`openssl rand -base64 32`). Seals the signing secrets in the database. Environment only |
| _(none)_            | `BOWLER_ENCRYPTION_KEY_PREVIOUS` | off        | The key being replaced, while rotating. Environment only                                      |
| `--addr`            | `BOWLER_ADDR`            | `127.0.0.1:8080`   | Where to listen. Use `0.0.0.0:8080` only behind a proxy                                       |
| `--db`              | `BOWLER_DB`              | `bowler.db`        | The SQLite file                                                                               |
| `--workers`         | `BOWLER_WORKERS`         | `128`              | Most deliveries in progress at once                                                           |
| `--timeout`         | `BOWLER_TIMEOUT`         | `15s`              | How long one attempt may take                                                                 |
| `--lease`           | `BOWLER_LEASE`           | timeout + 45s      | How long work taken by an instance that dies waits for another to take it                     |
| `--drain`           | `BOWLER_DRAIN`           | `10s`              | How long shutdown lets attempts finish                                                        |
| `--max-payload`     | `BOWLER_MAX_PAYLOAD`     | `262144`           | Largest message payload, in bytes                                                             |
| `--retention`       | `BOWLER_RETENTION`       | `720h` (30 days)   | How long finished messages and their history are kept. `0` for ever, otherwise at least `24h` |
| `--audit-retention` | `BOWLER_AUDIT_RETENTION` | `8760h` (365 days) | How long the audit log is kept. `0` for ever, otherwise at least `168h`                       |
| `--log-format`      | `BOWLER_LOG_FORMAT`      | `json`             | `json` or `text`                                                                              |
| `--log-level`       | `BOWLER_LOG_LEVEL`       | `info`             | `debug`, `info`, `warn` or `error`                                                            |
| `--allow-cidr`      | `BOWLER_ALLOW_CIDRS`     | none               | A private range endpoints may use, such as `10.20.0.0/16`. Repeat it or separate with commas  |
| `--allow-private`   | `BOWLER_ALLOW_PRIVATE`   | off                | Development only. Allows private and loopback addresses for every endpoint                    |
| `--allow-http`      | `BOWLER_ALLOW_HTTP`      | off                | Development only. Allows plain `http`                                                         |

## Private receivers

A receiver on your own network, say `10.20.1.7`, is refused by default, like every private address. Name its range:

```bash
bowler serve --allow-cidr 10.20.0.0/16 --allow-cidr 192.168.5.0/24
# or: BOWLER_ALLOW_CIDRS=10.20.0.0/16,192.168.5.0/24
```

Every other private address stays blocked, and the start-up log lists the open ranges. Things to know:

- **It applies to every customer.** Any app that can register an endpoint can point it into an allowed range, so allow only a range where that is acceptable.
- **Bowler refuses to start** if a range reaches public addresses, includes a cloud metadata address such as `169.254.169.254`, is not written as a network (`10.20.0.5/16`), or lies outside the private ranges. The message says which.
- **Write the address, not `localhost`.** A URL that names `localhost` is refused at registration even when `127.0.0.0/8` is allowed.
- **A change needs a restart.**

The reasoning is in [decision 0012](decisions/0012-operator-allow-list-for-private-receivers.md).

## Encrypting the signing secrets

Set `BOWLER_ENCRYPTION_KEY` and the signing secrets are sealed with AES-256-GCM in the database file, so a copy of the file or a backup cannot forge webhooks. The API and the senders still see the plain secret. The reasoning and the costs are in [decision 0010](decisions/0010-encrypted-signing-secrets.md).

```bash
export BOWLER_ENCRYPTION_KEY=$(openssl rand -base64 32)
go run ./cmd/bowler serve
```

- **Turning it on for an existing database.** Start with the key set. Secrets that are still plain are sealed, and the file is compacted so no plain text stays in it.
- **Rotating the key.** Start once with the new key as `BOWLER_ENCRYPTION_KEY` and the old one as `BOWLER_ENCRYPTION_KEY_PREVIOUS`. Everything is sealed again under the new key. From the next start the old key can be dropped, and the program refuses to start with only the old one.
- **A missing or wrong key** stops the program at start, with a message that names the setting.
- **Keep the key apart from the database and its backups, and back it up.** If it is lost, the secrets are lost and the program will not start. Backups from before encryption was turned on still hold the plain secrets.
- Payloads are not encrypted.

## Retention

Finished messages are deleted with their deliveries and attempts after `--retention` (30 days by default). A message that still has work to do is never deleted. The audit log has its own period, `--audit-retention` (365 days). Both can be set to `0` to keep everything. See [decision 0008](decisions/0008-retention.md). Deleted rows do not shrink the file: the space is reused, and shrinking it needs a `VACUUM`.
