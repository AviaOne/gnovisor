# GnoVisor

**GnoVisor, by AviaOne.com**

An upgrade supervisor for **gno.land** nodes: the gno.land counterpart of
Cosmovisor.

GnoVisor starts your `gnoland` node, watches the chain for a coordinated halt
programmed by GovDAO, prepares the new version ahead of time, and at the halt
backs the node up, switches it to the new version and starts it again, with no
one at the keyboard. It serves gno.land only. It is written from scratch, in
Go, with no dependency outside the Go standard library, and published by
[AviaOne.com](https://aviaone.com).

Released under semantic tags. The current release is on the
[releases page](https://github.com/AviaOne/gnovisor/releases).

---

## Why a supervisor is needed on gno.land

Cosmovisor does not work here, and the reasons are in the chain itself:

| On a Cosmos chain | On gno.land | What GnoVisor does |
|---|---|---|
| The upgrade module writes `data/upgrade-info.json`, which Cosmovisor watches | Nothing writes that file. A halt is two chain parameters, `node:p:halt_height` and `node:p:halt_min_version`, set by a GovDAO proposal | Reads both parameters from the node's RPC |
| The node process exits at the upgrade height | **The process stays alive at a halt.** Consensus stops, the RPC stays open, and a supervisor waiting for the process to exit waits forever | Sees a halt as a node that stays at the halt height with no new block |
| The binary is all a version needs | **A version is a binary and its `GNOROOT`.** The node reads the standard libraries from `GNOROOT/gnovm/stdlibs`, and the validator guide asks for a `GNOROOT` at the same tag as the binary | Prepares and keeps both, per version, and starts the node with the matching `GNOROOT` |
| Upgrade plans name the binaries to download | gno.land publishes a ledger, `misc/deployments/<chain>/upgrades.json`: every version the network ran, its commit, its halt height, one download per platform with its sha256, and its container image with its digest | Reads the ledger, takes the binary from the image it names and checks every download against it |
| An old binary is refused past the upgrade height | **With no version floor, nothing stops the old binary from resuming the chain after a halt.** Every halt on `gnoland-1` so far was set without a floor | Never starts the old binary again after a halt |

It also covers two situations an operator usually handles by hand:

- **Syncing from genesis stops at every past halt.** Each one fires again
  during replay, and the node needs a restart, on the version the ledger names
  for the blocks that follow. GnoVisor does it at each of them.
- **Rolling releases.** The ledger may add a patch on the same minor version,
  with no halt, that operators install when they choose. GnoVisor installs it
  inside a daily time window you set, so that validators do not all restart
  together.

---

## What GnoVisor does, step by step

1. **A halt is programmed.** GnoVisor reads the halt height from the node and
   looks it up in the ledger. As soon as the entry is there, it prepares that
   version: binary taken over HTTPS from the version's official image and
   checked against the ledger's image digest, `GNOROOT` cloned at the
   version's tag and its commit compared with the ledger's. A preparation
   that stops halfway never leaves a version that looks ready.
2. **The halt is reached.** As soon as the node reports the halt height, the
   halt block is committed and the node will produce nothing more, so there
   is nothing to wait for. GnoVisor stops the node (SIGTERM, then SIGKILL
   after `shutdown_grace`), backs up the node directory, points `current` at
   the new version, and starts the node again. The node is down for the stop,
   the backup and the start: on a large node directory the backup is the
   longest part, and `backup = false` removes it.
   GnoVisor reads the ledger again at that moment, so the version it switches
   to is the one the ledger names now.
3. **The chain resumes.** GnoVisor waits for the first block after the halt,
   for at most `restart_timeout`, and logs that the update succeeded.

When something goes wrong, the rule is always the same: **the node is left
stopped, and GnoVisor never goes back to the old version.**

- **The version is not in the ledger yet when the halt is reached.** The node
  is stopped and stays stopped. GnoVisor reads the ledger again every minute
  until the entry appears, then switches. A version you add by hand during
  the wait (see [Adding a version by hand](#adding-a-version-by-hand)) is
  taken within the same minute. The halt is written to
  `gnovisor/state.json` first, so a restart of GnoVisor, or of the whole
  machine, never starts the old binary in the meantime.
- **The new version does not produce a block in time, or refuses to start**
  (an app hash mismatch, for example). The node is stopped, the exact error
  is logged, and GnoVisor keeps watching: it reads the ledger again every
  minute, and as soon as gno.land names another version for the blocks after
  that halt (a fix is a new tag), it prepares it and switches again. A
  version you add by hand for that halt is taken the same way. The halt stays
  pending in `gnovisor/state.json`, so a restart of GnoVisor resumes the
  watch. The backup and the old version are left in place.
- **The backup fails**, a full disk for example. No switch happens, the node
  stays stopped, the error is logged. An update without the backup you asked
  for does not go ahead.
- **The node exits by itself**, outside any update. GnoVisor logs the exit
  code and exits with an error, and systemd starts it again. It does not
  restart a crashing node itself, so that a crash loop stays visible.

And a few things GnoVisor deliberately leaves alone:

- **A node stopped by its own configuration**, a `halt_height` in
  `config.toml` for example, matches neither the chain parameter nor the
  ledger. GnoVisor logs a warning and touches nothing: it is not an upgrade.
- **`secrets/` is never copied** into a backup. A copy of the validator key
  restored somewhere else is a double sign waiting to happen.
- **A backup is never deleted**, and an existing one is never overwritten.
- **A halt that needs a genesis replay** (`gnogenesis fork`) is out of scope.
  It stays a manual operation.
- **No vote, no proposal**, and nothing is written to the chain.

---

## Installation on Linux (Ubuntu / Debian)

### Requirements

- **A gno.land node that already runs**, synced or syncing, with its node
  directory (the `--data-dir` of `gnoland start`).
- Linux, amd64 or arm64.
- **Git**, used to prepare the `GNOROOT` of every version:

  ```bash
  git --version
  ```

- **Outbound HTTPS** to `github.com` and `raw.githubusercontent.com`, for the
  ledger and the `GNOROOT` clones, and to `ghcr.io` and the storage it
  redirects to, for the binaries. No Docker is needed.
- **The default database of the node**, `db_backend = "pebbledb"` in its
  `config.toml` (or `goleveldb`). The binary GnoVisor runs is built without
  CGO, so it has no `lmdbdb` or `mdbxdb` backend (see
  [Where the binary comes from](#where-the-binary-comes-from)):

  ```bash
  grep -n '^db_backend' ${NODE_HOME}/config/config.toml
  ```

- **Disk space for the backups.** A backup is a full copy of the node
  directory minus `secrets/`, taken at every update, and GnoVisor never
  deletes one. Watch the disk, or set `backup = false`.
- **No Go installation is needed.** The release binary of step 2 is
  self-contained. Go is only needed if you choose to build GnoVisor from
  source yourself (step 2, option B). GnoVisor never builds `gnoland`, so the
  Go installed on the machine, if any, has no effect on GnoVisor or on the
  node.

### Step 1 - Describe your node

Every command below uses shell variables, so you can copy and paste them
without editing anything. Set them once, in the terminal you are working in:

```bash
export CHAIN_ID=gnoland-1
export NODE_USER=gnoland
export NODE_HOME=/home/gnoland/gnoland-data
export GNO_VERSION=v1.5.0
```

- `CHAIN_ID` is the chain the node serves: `gnoland-1` for mainnet, `onyx-1`
  for the testnet. GnoVisor knows where the ledger of these two lives; for any
  other chain you will pass `-ledger-url` in step 3 and set `ledger_url` in
  step 4.
- `NODE_USER` is the system account that runs your node today.
- `NODE_HOME` is the node directory, the one holding `config/`, `secrets/`
  and `db/`.
- `GNO_VERSION` is the version your node runs **right now**. To see the
  versions of your chain and the halts between them, read the `UPGRADES.md`
  next to its ledger, for example
  [the gnoland-1 upgrade table](https://github.com/gnolang/gno/blob/master/misc/deployments/mainnet.gno.land/UPGRADES.md).

**These variables only live in the terminal you set them in.** If you close it
or open a new one, run the `export` lines again before continuing.

> **Syncing a new node from genesis?** Set `GNO_VERSION` to the genesis
> version of the ledger, `v1.2.0` on `gnoland-1`. GnoVisor then takes the node
> through every past halt, version by version, as the ledger describes them.

### Step 2 - Install the binary

Two ways, with the same result. **Option A is the usual one and needs no Go.**

**Option A - Download the release binary (recommended, no Go needed).** Set
`GNOVISOR_VERSION` to the newest tag on the
[releases page](https://github.com/AviaOne/gnovisor/releases):

```bash
GNOVISOR_VERSION=v1.0.0
GNOVISOR_ARCH=amd64   # arm64 on an ARM server
mkdir -p /tmp/gnovisor-dl && cd /tmp/gnovisor-dl
curl -fsSLO https://github.com/AviaOne/gnovisor/releases/download/${GNOVISOR_VERSION}/gnovisor-${GNOVISOR_VERSION}-linux-${GNOVISOR_ARCH}.tar.gz
curl -fsSLO https://github.com/AviaOne/gnovisor/releases/download/${GNOVISOR_VERSION}/SHA256SUMS
grep " gnovisor-${GNOVISOR_VERSION}-linux-${GNOVISOR_ARCH}.tar.gz$" SHA256SUMS | sha256sum -c -
tar -xzf gnovisor-${GNOVISOR_VERSION}-linux-${GNOVISOR_ARCH}.tar.gz
sudo install -m 0755 /tmp/gnovisor-dl/gnovisor /usr/local/bin/gnovisor
gnovisor version
```

The `sha256sum` command must print
`gnovisor-v1.0.0-linux-amd64.tar.gz: OK` (with your version and
architecture). `/tmp/gnovisor-dl` may be removed afterwards.

**Option B - Build from source (only if you want to compile it yourself).**
This is the only case that needs Go: **Go 1.26.8 or later**. First check
whether a usable Go is already installed:

```bash
go version
```

A Go 1.21 or later reads the version this project asks for and downloads it by
itself, if your environment allows Go to fetch toolchains. Otherwise install Go
into its own versioned directory. **Nothing existing is removed**, so any Go
already present on the machine, and anything depending on it, keeps working:

```bash
GO_VER=1.26.8
GO_ARCH=amd64   # arm64 on an ARM server
curl -fsSL https://go.dev/dl/go${GO_VER}.linux-${GO_ARCH}.tar.gz -o /tmp/go.tgz
sudo mkdir -p /usr/local/go${GO_VER}
sudo tar -C /usr/local/go${GO_VER} --strip-components=1 -xzf /tmp/go.tgz
/usr/local/go${GO_VER}/bin/go version
```

Use it for this session only, without touching any profile file, then build
and install:

```bash
export PATH=/usr/local/go${GO_VER}/bin:$PATH
cd ~ && git clone https://github.com/AviaOne/gnovisor && cd gnovisor && make build
sudo install -m 0755 ~/gnovisor/build/gnovisor /usr/local/bin/gnovisor
gnovisor version
```

Once built, the binary is self-contained: you may build on one machine and
copy it to another of the same architecture.

Either way, the binary is now at `/usr/local/bin/gnovisor`. A release binary,
or a binary built from a tag, reports that tag without the leading `v`.

### Step 3 - Initialise GnoVisor in the node directory

```bash
sudo -u ${NODE_USER} gnovisor init -home ${NODE_HOME} -chain-id ${CHAIN_ID} -version ${GNO_VERSION}
```

That command:

- reads the ledger of the chain and finds `${GNO_VERSION}` in it;
- takes the `gnoland` binary of that version from its official image (see
  [Where the binary comes from](#where-the-binary-comes-from)), checked
  against the image digest the ledger gives;
- clones the `GNOROOT` at the version's tag and checks its commit against
  the ledger;
- creates `${NODE_HOME}/gnovisor/` with both in `genesis/`, and `current`
  pointing at it;
- writes `gnovisor/gnovisor.toml`, for you to complete.

It changes nothing else in the node directory, and it refuses to run if
`gnovisor/` already exists. The binary your node runs today is not used and
not touched: from now on the node runs the one in `gnovisor/`.

On a chain other than `gnoland-1` and `onyx-1`, pass the ledger with
`-ledger-url <url or file>`.

**Alternative: start from files you already have.** Instead of `-version`,
`-binary <file> -gnoroot <dir>` takes a `gnoland` binary and a `GNOROOT` you
downloaded yourself. The binary must then be the release binary the ledger
lists (it is recognised by its sha256), and the `GNOROOT` a Git clone at the
same version's commit. The release binary needs glibc 2.38 or later.

### Step 4 - Configure

```bash
sudo -u ${NODE_USER} nano ${NODE_HOME}/gnovisor/gnovisor.toml
```

Two settings must be filled in.

**`node_args`**: the arguments your node is started with today, after
`gnoland start`, as a list of strings. Take them from your current systemd
unit, **minus `--data-dir`**, which GnoVisor adds itself from the node
directory and refuses in this list. Write paths in full. The mainnet
validator guide starts the node with `--chainid gnoland-1 --genesis
genesis.json --skip-genesis-sig-verification`, which gives, for example:

```toml
node_args = ["--chainid", "gnoland-1", "--genesis", "/home/gnoland/genesis.json", "--skip-genesis-sig-verification"]
```

**`rolling_window`**: the daily time range, **in UTC**, in which GnoVisor may
install a rolling release. There is no default on purpose: a value shared by
every operator would restart their nodes at the same minute, and the chain
cannot produce a block while validators holding a third of the voting power are
down. Pick your own, at a time you are usually around:

```toml
rolling_window = "03:20-04:10"
```

A range may cross midnight (`"23:30-00:30"`).

Every other key has a working default. The full reference is in the
[Configuration](#configuration) section below.

### Step 5 - Hand the node over to GnoVisor

From now on GnoVisor starts the node. **Your current node service must be
stopped and disabled**, otherwise two processes would run on the same node
directory. Replace `gnoland` with the name of your current service:

```bash
sudo systemctl stop gnoland
sudo systemctl disable gnoland
```

Create the GnoVisor unit. This writes it for you, filled in with your values:

```bash
sudo tee /etc/systemd/system/gnovisor-${CHAIN_ID}.service > /dev/null <<EOF
[Unit]
Description=gnovisor - gno.land node supervisor for ${CHAIN_ID}
After=network-online.target
Wants=network-online.target

[Service]
User=${NODE_USER}
Group=${NODE_USER}
Type=simple
ExecStart=/usr/local/bin/gnovisor run -home ${NODE_HOME}
Restart=always
RestartSec=5
TimeoutStopSec=180
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
```

`TimeoutStopSec` must stay above `shutdown_grace` (two minutes by default):
when systemd stops GnoVisor, GnoVisor first stops the node and gives it that
long to exit cleanly.

Do not set `GNOROOT` in the unit. GnoVisor sets it for the node, to the
`GNOROOT` of the version it starts.

Then enable and start it:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now gnovisor-${CHAIN_ID}
sudo systemctl status gnovisor-${CHAIN_ID}
```

### Step 6 - Check that it works

Watch the log. The node's own output goes through unchanged, and GnoVisor's
lines stand apart, each starting with `gnovisor` and a UTC timestamp:

```bash
sudo journalctl -u gnovisor-${CHAIN_ID} --no-hostname -f
```

You should see `starting, GnoVisor, by AviaOne.com, node version ...`, then
`node started, version ..., pid ...`, then the node producing or syncing
blocks.

Ask GnoVisor what it knows:

```bash
sudo -u ${NODE_USER} gnovisor status -home ${NODE_HOME}
```

It prints the version in service, the last halt it handled, the node height,
and, when a halt is programmed, its height and whether its version is already
prepared.

### Before a halt

Nothing is required: GnoVisor prepares the version as soon as the ledger lists
it. To prepare it right away instead of at the next check:

```bash
sudo -u ${NODE_USER} gnovisor prepare -home ${NODE_HOME}
```

To provide the binary yourself, put it at
`${NODE_HOME}/gnovisor/upgrades/<version>/bin/gnoland` before the halt. It is
used first, if its sha256 is the ledger's, and nothing is downloaded. This is
the only way when `auto_download = false`.

### Adding a version by hand

A version the ledger does not list can be given to GnoVisor directly. The case
is rare: a security fix handed to validators privately, before the flaw is
disclosed, which the public ledger cannot list. Without it, a node halted for
that fix would stay stopped, since GnoVisor never restarts the old binary
after a halt.

You need the binary and its `GNOROOT`, as you received them. For a fix that
comes with a GovDAO halt, give the halt height:

```bash
sudo -u ${NODE_USER} gnovisor add-upgrade -home ${NODE_HOME} \
  -version v1.6.1 -binary /path/to/gnoland -gnoroot /path/to/gnoroot \
  -halt 250000
```

For a fix that needs no halt, on the same `MAJOR.MINOR` as the version in
service, use `-rolling` instead of `-halt`. It is installed inside
`rolling_window`, or at the next check with `-now`:

```bash
sudo -u ${NODE_USER} gnovisor add-upgrade -home ${NODE_HOME} \
  -version v1.5.1 -binary /path/to/gnoland -gnoroot /path/to/gnoroot \
  -rolling -now
```

The command copies both into `gnovisor/upgrades/<version>/`, records the
sha256 of the binary, and lists the version in `gnovisor/local-upgrades.json`.
The service does not need a restart: GnoVisor reads that file at every check.
`gnovisor status` lists the versions added this way.

What GnoVisor does with them:

- **At a halt the ledger does not list**, it switches to the version added for
  that height.
- **When the ledger and a version added by hand name two different versions,
  or two different binaries, for the same halt**, nothing says which one is
  right: GnoVisor does not switch, keeps the node stopped and logs both.
- **You vouch for what you add.** Nothing published can confirm a private
  binary, and its `GNOROOT` is not checked against Git, since a private fix
  may have no public tag. Check the binary's sha256 against the one announced
  in the channel it came from.
- A rolling version is not installed while a halt is programmed or while the
  node is catching up, as with the rolling releases of the ledger.

### After a failed update

- **The new version failed to start or to produce a block.** GnoVisor logs
  `waiting for a version other than <version>`, keeps the node stopped and
  watches the ledger: a fix gno.land publishes for that halt is taken without
  you. If the cause is on your side (configuration, disk), fix it and restart
  the service: GnoVisor tries the version again.
- **A rolling release failed.** GnoVisor logs `waiting for an operator` and
  keeps the node stopped. Read the error above that line, fix the cause and
  restart the service.
- **The backup failed.** Free the space, then restart the service: the halt is
  still pending in `gnovisor/state.json`, so GnoVisor tries the update again
  and never starts the old binary.

```bash
sudo systemctl restart gnovisor-${CHAIN_ID}
```

Every backup is under `${NODE_HOME}/gnovisor/backups/<height>-<version>/`,
named after the halt height and the version that produced the data. To
restore one, stop the service and copy the backup's content back over the node
directory, leaving `current` on the new version: a backup holds the chain at
the halt height, and the old version must never run past a halt, since with no
version floor it could resume the chain on its own. `secrets/` is not in any
backup and does not need restoring.

### Service management

```bash
# Start
sudo systemctl start gnovisor-${CHAIN_ID}

# Stop (stops the node too)
sudo systemctl stop gnovisor-${CHAIN_ID}

# Restart (restarts the node too)
sudo systemctl restart gnovisor-${CHAIN_ID}

# Status
sudo systemctl status gnovisor-${CHAIN_ID}

# Live logs
sudo journalctl -u gnovisor-${CHAIN_ID} --no-hostname -f
```

Stopping GnoVisor always stops the node. A switch already under way is not
interrupted by a stop request until the new version is in place.

### Updating GnoVisor

Only the GnoVisor binary is replaced. The node directory, the versions, the
backups and `gnovisor.toml` are files on disk that the update never touches.
**Restarting GnoVisor restarts the node**, so do not do it while a halt is
close.

First get the new binary, the same way as in step 2:

- **Option A, release binary:** run the download, check and `tar` commands of
  step 2 with the new `GNOVISOR_VERSION`, without the `sudo install` line.
  The new binary is then `/tmp/gnovisor-dl/gnovisor`.
- **Option B, from source:** check out the newest tag and build it. The new
  binary is then `~/gnovisor/build/gnovisor`.

  ```bash
  cd ~/gnovisor && git fetch --tags && git checkout "$(git tag --sort=-v:refname | head -n1)" && make build
  ```

Then replace it, using the path of the option you chose:

```bash
sudo systemctl stop gnovisor-${CHAIN_ID}
sudo install -m 0755 /tmp/gnovisor-dl/gnovisor /usr/local/bin/gnovisor   # option B: ~/gnovisor/build/gnovisor
sudo systemctl start gnovisor-${CHAIN_ID}
gnovisor version
```

The last command prints the tag of the new binary.

### Supervising more nodes

One GnoVisor process supervises one node. For a second node on the same
machine, go back to **step 1**, set the variables to that node, and run the
steps again: it gets its own `gnovisor/` directory, its own configuration and
its own service.

---

## Where the binary comes from

For every version, the gno.land ledger names two builds of `gnoland`: the
binaries attached to the release page, and the container image
`ghcr.io/gnolang/gno/gnoland:<version>` with its digest. They differ:

| | Release binary | Binary of the image |
|---|---|---|
| Built by | `release-chain-tag.yml`, with CGO | the `Dockerfile`, `CGO_ENABLED=0` |
| Linked | dynamically, needs **glibc 2.38** or later | statically, runs on any Linux |
| Ubuntu 22.04 (glibc 2.35) | does not start: `GLIBC_2.38' not found` | starts |
| `db_backend` `lmdbdb`, `mdbxdb` | available | not available |

GnoVisor takes the binary of the image, so that it runs on every validator's
system. It downloads the image over HTTPS, without Docker, checks the image
index against the digest the ledger gives and every manifest and layer
against its own digest, and extracts `/usr/bin/gnoland`. Checked on the
`linux/amd64` images of `gnoland-1` v1.2.0 to v1.5.0: each holds a
statically linked binary reporting its version.

gno.land fills the digest of an image once its CI has built it, which can
come after the version is listed. Until then GnoVisor reads the image by its
tag, `ghcr.io/gnolang/gno/gnoland:<version>`, which gno.land never re-pushes
(its `RELEASING.md`), and records the digest it read. When the ledger then
gives a digest, the two are compared; if the ledger names another image for
that version, the version is prepared again from it before the halt.

At the halt, GnoVisor reads the ledger again before switching, so that a
version gno.land names at the last minute (a fix is always a new tag) is the
one that runs.

---

## Files

Everything GnoVisor owns lives in `gnovisor/`, inside the node directory. The
layout follows Cosmovisor's, so a validator finds their way, with one change:
a version directory is named by its tag, because the gno.land ledger orders
versions by tag and has no upgrade plan names.

```
<node directory>/
    config/  secrets/  db/          the node, untouched
    gnovisor/
        gnovisor.toml               the configuration
        genesis/                    the version given to gnovisor init
            bin/gnoland
            gnoroot/
            version.json            version, commit, sha256 of the binary, image digest
        upgrades/
            v1.6.0/
                bin/gnoland
                gnoroot/
                version.json
        current -> upgrades/v1.6.0  the version in service
        backups/
            <height>-<version>/     one per update
        state.json                  last halt handled, halt pending
        local-upgrades.json         versions added by hand, if any
```

At every start GnoVisor checks the binary of `current` against its
`version.json`, and refuses to start on a mismatch.

---

## Configuration

`gnovisor/gnovisor.toml`. GnoVisor reads no environment variable for its
configuration. An unknown key is refused, with its name, so a typo cannot pass
for a default.

| key | default | what it does |
|---|---|---|
| `node_dir` | written by `init`, required | the node directory, absolute. Must be the `-home` given to the command |
| `node_args` | required | arguments passed to `gnoland start`, as a list of strings. `--data-dir` is added by GnoVisor and refused here |
| `chain_id` | written by `init`, required | must match the `network` the node reports and the `chain_id` of the ledger, otherwise GnoVisor refuses to act |
| `rolling_window` | required, no default | daily UTC range `HH:MM-HH:MM` in which a rolling release may be installed. May cross midnight |
| `rpc` | `http://127.0.0.1:26657` | RPC of the supervised node |
| `ledger_url` | the chain's ledger on the `chain/mainnet` branch of `gnolang/gno`, for `gnoland-1` and `onyx-1` | where the ledger is read. A local file path is accepted. Required for any other chain |
| `gnoroot_repo` | `https://github.com/gnolang/gno.git` | Git repository each `GNOROOT` is cloned from |
| `poll_interval` | `5s` | how often the node is queried |
| `auto_download` | `true` | download a version that is not prepared. With `false`, the binary must be deposited by hand (see [Before a halt](#before-a-halt)) |
| `backup` | `true` | back up the node directory before every switch |
| `backup_dir` | `backups` | where backups go, relative to `gnovisor/` or absolute |
| `shutdown_grace` | `2m` | time between SIGTERM and SIGKILL when stopping the node |
| `halt_confirm` | `30s` | time without a new block, at a height that is neither the chain's halt nor a ledger halt, before GnoVisor reports a node stopped by its own configuration. A known halt is handled at once |
| `restart_timeout` | `10m` | longest wait for the first block after a switch, and for the RPC at start |

Durations are written as `"30s"`, `"2m"`, `"1h"`.

The ledger is read at start, then every 10 minutes, and every minute while a
reached halt waits for its version. A ledger that cannot be read, or that
breaks one of gno.land's own ledger rules, is ignored with an error in the log,
and the last valid one is kept.

### Commands

```
gnovisor init     -home <dir> -binary <gnoland> -gnoroot <dir> -chain-id <id> [-ledger-url <url|file>]
gnovisor run      -home <dir>    run and supervise the node, the ExecStart of the service
gnovisor status   -home <dir>    version in service, last halt, node height, programmed halt
gnovisor prepare  -home <dir>    prepare the version of the programmed halt now
gnovisor add-upgrade -home <dir> -version <vX.Y.Z> -binary <gnoland> -gnoroot <dir> (-halt <height> | -rolling [-now])
                                 add a version the ledger does not list
gnovisor version                 print the version of GnoVisor
```

### Version

```bash
gnovisor version
```

A release binary reports the tag it was built from, without the leading `v`.
A binary built from a working copy reports the value compiled into the source.

---

## Building and testing

```bash
make build      # build/gnovisor
make test       # go test -race ./...
make lint       # golangci-lint
```

The tests include the real ledgers of `gnoland-1` and `onyx-1`, every ledger
rule gno.land's own tests check, and a full simulation with a fake node that
produces blocks and stops at a halt with its RPC open, as a real one does:
scheduled halt, missing version, restart of GnoVisor with a pending halt, sync
from genesis across several halts, local halt, failed restart, failed backup,
crash of the node, rolling release inside and outside its window, versions
added by hand (at a pending halt, ahead of a halt, in conflict with the
ledger, rolling with and without `-now`), a wrong chain, an image digest the
ledger does not give yet, another digest named later, and a version replaced
in the ledger shortly before the halt. A fake registry serves the images as ghcr.io
does (anonymous token, redirect to storage, index with an attestation
manifest), and a tampered layer is refused.

---

## About AviaOne

[AviaOne](https://aviaone.com) has been a professional Cosmos validator for over
three years, and builds tooling for the ecosystem rather than for itself alone.

Its main contribution is
**[ABS, AviaOne BlockChains Service](https://aviaone.com/blockchains-service/)**,
a live directory covering more than 350 blockchains. ABS turns raw Chain Registry
data into something operators can rely on: RPC endpoints tested every three hours
and ranked by latency, block explorers rechecked daily so dead links disappear,
the binary version genuinely running on each network compared against what the
registry claims, and live network status.

GnoVisor comes from the same place. Its own maintainers also publish
[GnoDuty](https://github.com/AviaOne/GnoDuty) and
[Tenderseed](https://github.com/AviaOne/tenderseed) for gno.land operators.

## Credits

- **Directory layout**: follows
  [Cosmovisor](https://github.com/cosmos/cosmos-sdk/tree/main/tools/cosmovisor),
  so that validators find their way. No code was taken from it.
- **Ledger format and rules**: defined by gno.land in
  [`gno.land/pkg/upgrades`](https://github.com/gnolang/gno/tree/master/gno.land/pkg/upgrades).
  GnoVisor imports no gno.land code; it reads the ledger as data and applies
  the same rules.
- **Author**: [AviaOne.com](https://aviaone.com).

## Disclaimer

GnoVisor is maintained by AviaOne.com alone, with no funding or sponsorship.
We aim for quality but cannot promise the response times of a funded project.
If you hit a bug, open an issue and we will do our best.

## License

GNU Affero General Public License v3.0, in [LICENSE.md](LICENSE.md), with
additional terms under its section 7, in [NOTICE.md](NOTICE.md): every copy and
every modified version, whatever its name, keeps the notice "GnoVisor, by
AviaOne.com" in the source files, in `NOTICE.md` and in what the program
displays, and a modified version is marked as different from the original.
