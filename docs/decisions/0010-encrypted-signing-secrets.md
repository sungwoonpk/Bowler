# 0010. Signing secrets are encrypted in the database when a key is given

Status: accepted

## Context

An endpoint's signing secret has to be readable by the program, because it signs every request with it. So it was stored in
the clear, and anyone who got a copy of the SQLite file, from a backup, a disk image or a stray volume, could forge webhooks
to every endpoint in it. The threat model listed this as its first gap.

## Decision

- If `BOWLER_ENCRYPTION_KEY` is set, secrets are stored **sealed with AES-256-GCM**. The key is 32 bytes written as base64,
  as `openssl rand -base64 32` makes it. Like the API key it can only come from the **environment**, never from a flag, and
  it is never printed, not even in an error about itself.
- A sealed value reads `enc:v1:<key id>:<nonce and ciphertext>`. The key id is the start of a hash of the key, so a value says
  which key opened it and reveals nothing about the key. The nonce is random for every value, so equal secrets do not look equal.
- A sealed value is **tied to its row** (the endpoint id and the column are authenticated along with it). A ciphertext copied
  into another endpoint's row does not open, instead of silently signing with the wrong secret. A test does exactly that.
- **Nothing changes for callers.** The API still returns the plain secret when it is made and when it is read, and the
  dispatcher still signs with the plain one. Only what is in the file changes.
- **On every start** the program looks at every secret. A plain one, from before encryption, and one sealed under an older key
  are written again under the current key. This one pass does three jobs: turning encryption on for an existing database,
  rotating the key, and checking the key.
- It **opens every secret while doing it**, so a missing key, a wrong key, or a damaged or moved value stops the program at
  start with a message that names the setting to fix. Without this, the first sign of a problem would be an endpoint that
  suddenly cannot be delivered to.
- After it changes anything, the program **compacts the database** (a checkpoint and `VACUUM`). Replacing a value leaves the
  old text in unused pages and in the write-ahead log until they are reused, so without this the plain secret would still be
  in the file. A test checks the file and its journal for the plain text, and fails if the compaction is left out.
- **Rotation**: set `BOWLER_ENCRYPTION_KEY` to the new key and `BOWLER_ENCRYPTION_KEY_PREVIOUS` to the old one, and start. The
  secrets are sealed again under the new key. From the next start the old key can be dropped, and the program refuses to start
  with only the old key.
- With **no key**, nothing is encrypted and the program warns on every start. A database that already holds sealed secrets is
  refused without its key.
- The sealing sits behind a type with four methods, so a key manager could be put behind it later. None is built, and none was
  tried.

## Costs

- **The key is in the environment of the same process that holds the data.** This protects a copy of the file, a backup, a
  disk image. It does not protect against someone who can read the process's memory or environment, or who gets onto the
  machine and into the running program. For that, a key kept in a separate system is needed.
- **If the key is lost, the secrets are lost.** There is no recovery. The program will not start, and the endpoints would have
  to be made again. Keep the key somewhere safe and apart from the database and its backups.
- **Payloads and the first kilobyte of each receiver reply are not encrypted.** They may hold personal data. They are deleted
  after the retention period. This covers signing secrets only.
- **Old backups keep the plain secrets** if they were taken before encryption was turned on. Replace or destroy them.
- **The compaction needs free disk space about the size of the database**, and it holds the write lock while it runs. It only
  runs on a start that changed something, which is once for turning encryption on and once per rotation, and a start is the
  place where waiting is cheap.
- **A little more work on every read and claim**, for opening the secrets. `BenchmarkQueueEncrypted` runs the queue benchmark
  with sealing on. Run back to back four times each on one machine, plain gave about 6,960 to 7,400 deliveries a second and
  encrypted about 7,240 to 7,410, which is no difference beyond the noise of the machine.
- **Not a signature on the data.** A person with the file can still delete or replace a sealed value, which breaks that
  endpoint's signing and is noticed at the next start, and cannot be used to read or forge anything.

## Alternatives considered

- **A password-based key** (derive the key from a passphrase). The key here has 256 random bits, so a slow derivation adds
  nothing, and a passphrase would have to be typed or stored somewhere anyway.
- **Encrypt the whole database file** (SQLCipher, or a page-level cipher). Covers payloads too, and it needs a C library or a
  different driver, and Bowler's "one static file, no C compiler" is a point of the design ([0004](0004-sqlite-single-binary.md)).
  An encrypted volume does this at no cost to the code, and the documentation still recommends one.
- **Store only a hash of the secret.** It cannot be done, since the secret is needed to sign.
- **A separate key per endpoint**, wrapped by a master key. More to manage for no gain against the threat of a copied file.
