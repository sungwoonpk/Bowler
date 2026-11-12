# Security

## Reporting a problem

Please **do not open a public issue** for a security problem. Use GitHub's private reporting instead: on the repository page,
open the **Security** tab and choose **Report a vulnerability**. Include what you found, how to reproduce it and what you
think the impact is.

This is a personal project, so there is no service level. Reports are read and answered on a best-effort basis, and a
confirmed problem is fixed in `main`. Only the latest `main` is supported.

## What to expect from the service

[docs/threat-model.md](docs/threat-model.md) lists what Bowler defends against and, just as important, what it does not.
Read the "What is not covered" section before running it anywhere real. The short version:

- Signing secrets and payloads are stored **unencrypted** in the SQLite file.
- There is **one all-powerful API key**, and no TLS of its own.
- It has **not had an independent security review**.

## If you deploy it

- Make the API key with `openssl rand -hex 24`, and give it to the program only through the `BOWLER_API_KEY` environment
  variable. The program refuses keys shorter than 24 characters, and has no flag for it, because a flag shows up in the process
  list.
- Keep the database file on an encrypted volume that only the service can read. It holds every signing secret.
- Run Bowler behind a reverse proxy that terminates TLS, and listen on loopback or a private network. The default address is
  `127.0.0.1:8080`.
- Do **not** use `--allow-private` or `--allow-http` in production. The first removes the protection that stops users from making
  Bowler call your internal network.
- Prefer running the delivery workers where they cannot reach anything private at all. The built-in check is a second line of
  defense, not the first.
- Back up the database file. There is no replication.
- Watch `/metrics`, in particular `bowler_deliveries{state="failed"}` and `bowler_store_errors_total`.
