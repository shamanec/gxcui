# gxcui

Run XCUITests in parallel across booted simulators.

gxcui builds your test bundle once, asks `xcodebuild` what tests it contains, splits them
into batches, and runs each batch on a different booted simulator. When the run finishes it
merges the result bundles, writes a JUnit report and a self-contained HTML report, and
records how long every test took so the next run can split the work better.

> **Status: working.** Discovery, batching, parallel execution, retries and reporting all
> run end to end. Sharding across CI machines and re-running a previous run's failures are
> still to come — see [docs/PLAN.md](docs/PLAN.md).

**Contents** — [Requirements](#requirements) · [Install](#install) · [Quick start](#quick-start) ·
[How a run works](#how-a-run-works) · [Simulators](#simulators) · [Selecting tests](#selecting-tests) ·
[Batching strategies](#batching-strategies) · [Timings](#timings) · [Retries and flakiness](#retries-and-flakiness) ·
[Output](#output) · [Reports](#reports) · [Configuration reference](#configuration-reference) ·
[Command reference](#command-reference) · [CI](#ci) · [Troubleshooting](#troubleshooting) ·
[Development](#development)

---

## Requirements

- **A recent Xcode.** gxcui depends on `xcodebuild -enumerate-tests` and the
  `xcresulttool get test-results` command family, both relatively recent additions.
  Developed and verified against Xcode 26.6 (build 17F113), `xcresulttool` 24757, schema
  `0.1.0`; older Xcodes are untested and may lack these entirely.
- **Go 1.25 or newer**, to build it.
- **At least one iOS simulator**, booted yourself or by gxcui with
  [`bootSims`](#booting-simulators).

## Install

```bash
go build -o bin/gxcui ./cmd/gxcui
```

Or onto your `PATH`:

```bash
go install github.com/shamanec/gxcui/cmd/gxcui@latest
```

## Quick start

```bash
# 1. Boot the simulators you want to run on. gxcui uses every booted one by default,
#    or boots the ones you name for you — see "Booting simulators".
xcrun simctl boot 92F3C99D-476B-4BA5-B857-A7FAB6C60349
xcrun simctl boot 8442C46F-83D8-4A3B-8F34-47A4CE4C34D9

# 2. Check gxcui agrees about what it can use.
gxcui devices

# 3. Run.
gxcui run --workspace MyApp.xcworkspace --scheme MyAppUITests
```

Most people put the fixed parts in a config file and then just type `gxcui run`:

```yaml
# gxcui.yaml
version: 1
project:
  workspace: MyApp.xcworkspace
  scheme: MyAppUITests
retries:
  maxAttempts: 2
```

A run looks like this:

```
Building for testing…
Built .gxcui/dd/Build/Products/MyApp_MyAppUITests_iphonesimulator26.5-arm64.xctestrun
Found 128 test(s) on 2 simulator(s)
Planned 4 batch(es) across 2 simulator(s)
✓ batch-01 on xcpool-1 — 32 passed in 91s
✗ batch-02 on xcpool-2 — 30 passed, 2 failed in 104s
✓ batch-03 on xcpool-1 — 32 passed in 88s
✓ batch-04 on xcpool-2 — 32 passed in 96s
Attempt 2: retrying 2 test(s)
✓ batch-01-try2 on xcpool-1 — 1 passed in 12s
✗ batch-02-try2 on xcpool-2 — 0 passed, 1 failed in 11s
Writing reports…

────────────────────────────────────────────────────────────
128 test(s) in 214s on 2 simulator(s): 127 passed, 1 failed, 1 flaky

Failed (1):
  ✗ MyAppUITests/CheckoutTests/testApplyCoupon()  (xcpool-2)
      CheckoutTests.swift:88: XCTAssertTrue failed

Flaky (1) — passed only after retrying:
  ~ MyAppUITests/CheckoutTests/testGuestCheckout() (2 attempts)

Artifacts:
  report    .gxcui/runs/20260817-135615/report.html
  results   .gxcui/runs/20260817-135615/merged.xcresult
  junit     .gxcui/runs/20260817-135615/junit.xml
  manifest  .gxcui/runs/20260817-135615/run.json
  logs      .gxcui/runs/20260817-135615/logs

Re-run just these:
  gxcui run --include "MyAppUITests/CheckoutTests/testApplyCoupon()"
```

Before committing to a real run, `gxcui run --dry-run` prints the batch plan and the exact
`xcodebuild` command for each batch without running any tests. It still builds and
enumerates, because the plan depends on knowing what tests exist — point it at a prebuilt
`--xctestrun` if you want it to be instant.

## How a run works

```
build-for-testing (once)  →  .xctestrun  →  enumerate  →  filter  →  batch
                                                                       │
                                          ┌────────────────────────────┤
                                          ▼                            ▼
                                   worker: sim A                worker: sim B
                                   one batch at a time          one batch at a time
                                          └────────────┬───────────────┘
                                                       ▼
                            retry failures  →  merge  →  junit.xml + run.json + timings
```

The pivot of the design is **build once, run many**. `build-for-testing` produces an
`.xctestrun` file, and every batch then runs with `test-without-building -xctestrun`, which
compiles nothing. That is what makes the parallelism safe: any number of those can run at
once without contending on a build graph. Running batches straight from `-scheme` would put
every worker back into the same build.

Each batch is one `xcodebuild` invocation with an `-only-testing:` argument per test and its
own `-resultBundlePath`. `-parallel-testing-enabled NO` is always passed: gxcui *is* the
parallelism, and letting xcodebuild clone simulators as well would leave two schedulers
fighting over the machine.

A worker owns its simulator for the whole batch. gxcui never runs two batches on one
simulator at the same time, because a UI test needs exclusive control of the device.

## Simulators

**By default gxcui only consumes simulators that are already booted.** It never creates or
deletes one, and it boots, erases or shuts one down only when you ask it to. That is
deliberate: a test runner that manages simulator lifecycle can destroy a simulator someone
else was using, and recovering from that is tedious. Boot what you want with
`xcrun simctl boot <udid>`, and gxcui will use it — or hand the job to gxcui with
[`bootSims`](#booting-simulators), [`resetBefore` and `shutdownAfter`](#erasing-and-shutting-down).

The number of booted, eligible simulators is the concurrency of the run. Two booted
simulators means two batches at a time.

```bash
gxcui devices        # the simulators gxcui would use
gxcui devices --all  # plus every one it skipped, and why
```

```
$ gxcui devices --all
xcpool-1 (92F3C99D-476B-4BA5-B857-A7FAB6C60349) iOS 26.5
xcpool-2 (8442C46F-83D8-4A3B-8F34-47A4CE4C34D9) iOS 26.5

skipped (3):
  iPhone SE (3rd generation) (AD2B8BC3-…) iOS 18.1 — not booted
  slim-test (23097636-…) iOS 26.5 — not booted
  xcpool-3 (F103BA63-…) iOS 26.5 — excluded by simulators.exclude
```

Narrow the pool by UDID or by device name:

```yaml
simulators:
  include: [xcpool-1, xcpool-2]   # empty means every booted simulator
  exclude: [xcpool-3]             # exclude wins over include
```

or per invocation: `gxcui run --simulator xcpool-1 --simulator xcpool-2`.

### Booting simulators

Set `bootSims` and gxcui boots the simulators in `include` before the run starts, instead
of expecting them to already be up:

```yaml
simulators:
  include: [xcpool-1, xcpool-2, xcpool-3]
  bootSims: true
  bootTimeout: 5m
```

or `gxcui run --boot-sims`, which overrides the config either way — `--boot-sims=false`
turns it off for one run.

```
$ gxcui run --boot-sims
Booting 3 simulator(s): xcpool-1, xcpool-2, xcpool-3
Booted xcpool-2 (1/3)
Booted xcpool-1 (2/3)
Booted xcpool-3 (3/3)
Building for testing…
```

Worth knowing:

- **`include` is required.** An empty `include` means "every booted simulator", which names
  nothing to boot, so `bootSims` without it is a configuration error rather than a run that
  quietly boots nothing. A simulator in both `include` and `exclude` is not booted either:
  the run would refuse to use it anyway.
- **They boot at the same time.** A cold simulator takes tens of seconds to come up, and
  booting four in sequence would put minutes on the front of every run. Each gets its own
  `bootTimeout`; the run waits for all of them.
- **Each boot is `xcrun simctl bootstatus <device> -b`**, not `simctl boot`. `boot` returns
  as soon as the boot has *started*, and a test aimed at a half-booted simulator fails in
  ways that look like the test's own fault. `bootstatus` blocks until the device reports
  itself booted, and is safe to call on one that already is — so an already-running
  simulator is confirmed, not disturbed.
- **A simulator that will not boot fails the run**, and the error names every one that
  failed rather than only the first. You asked for that many; running with fewer would
  quietly mean less parallelism than you configured.
- **`--dry-run` never boots.** It prints what it would have booted and plans against
  whatever is up now.

How many should you boot? Reports of simulator instability past three or four concurrent
UI test runs on one machine are common, and the ceiling depends on your host's RAM and
cores far more than on gxcui. Start with two or three and increase while wall-clock time
still improves.

### Erasing and shutting down

Two more opt-in settings cover the rest of the simulator lifecycle:

```yaml
simulators:
  include: [xcpool-1, xcpool-2]
  bootSims: true
  resetBefore: true     # shut down + erase before the run; bootSims brings them up
  shutdownAfter: true   # shut down once the last batch finishes
```

or `gxcui run --reset-before --shutdown-after`, which override the config either way —
`--reset-before=false` turns it off for one run.

```
$ gxcui run --boot-sims --reset-before --shutdown-after
Erasing 2 simulator(s): xcpool-1, xcpool-2
Erased 2 simulator(s)
Booting 2 simulator(s): xcpool-1, xcpool-2
Booted xcpool-2 (1/2)
Booted xcpool-1 (2/2)
Building for testing…
…
Shutting down 2 simulator(s): xcpool-1, xcpool-2
Shut down 2 simulator(s)
Writing reports…
```

**`resetBefore` is `simctl shutdown` followed by `simctl erase`.** Erasing is what "clean"
means for a simulator — no installed app, no granted permissions, no keychain, nothing the
last run wrote — and it is the only way a suite starts from the same place every time.
simctl refuses to erase a booted device, hence the shutdown first.

- **It needs `bootSims`,** and gxcui says so rather than letting the run find out. Erasing
  leaves a simulator shut down, and booting is `bootSims`' job: a simulator gxcui did not
  boot is not one it will boot back. Without the pair, a reset would erase the pool the run
  was going to use and then fail with nothing booted — after the erasing. `bootSims` needs
  `include`, so a reset is always scoped to simulators you named.
- **It runs before anything else**, so the build and the enumeration already see clean
  devices.

**`shutdownAfter` is `simctl shutdown`, and nothing else.** It happens as soon as the last
batch is in, before the reports are written: those are built from result bundles on disk and
need no simulator, so there is no reason to hold a few gigabytes of RAM through the slowest
part of the run. A run interrupted with Ctrl-C still shuts its simulators down, and a
simulator that will not shut down is reported as a warning rather than failing the run — the
tests have already had their say.

**Both follow `include`.** The scope is the simulators you named, so nothing else on the
machine is touched:

| `include` | `exclude` | what it covers |
|---|---|---|
| set | — | those simulators, one `simctl` command each |
| empty | empty | **every simulator on the machine**, in one `simctl … all` |
| empty | set | every simulator except the excluded ones, one command each |

The last two rows only ever apply to `shutdownAfter`, since `resetBefore` requires
`bootSims` and so requires `include`. Read them twice anyway: an empty `include` means
"every booted simulator", so `shutdownAfter` with no `include` shuts down every simulator
the machine has, including ones that have nothing to do with your tests. On a CI worker that
is exactly what you want. On your own laptop, name the simulators. `--dry-run` prints the
full list under `reset:` and `shutdown:` before anything is touched, and `exclude` is
honoured in every case — an excluded simulator is one gxcui was told to keep its hands off.

## Selecting tests

gxcui discovers tests by asking xcodebuild, then applies your filters to the result. It
never guesses from file names.

```bash
gxcui enumerate                  # one identifier per line
gxcui enumerate --format tree    # grouped by target and class
gxcui enumerate --format json    # plus device, command, and what was filtered out
gxcui enumerate --verbose        # counts, and the tests that will not run
```

```
$ gxcui enumerate --format tree
└── MyAppUITests
    ├── LoginTests
    │   ├── testValidCredentials()
    │   └── testLockout()
    └── CheckoutTests
        ├── testApplyCoupon()
        └── testGuestCheckout()
```

Identifiers are `Target/Class/method()` — exactly the form `-only-testing:` accepts, so
`gxcui enumerate` output pipes straight into other tools.

### Patterns

`tests.include` and `tests.exclude` (and the `--include` / `--exclude` flags) match against
the whole identifier:

| Pattern | Meaning |
|---|---|
| `MyAppUITests/LoginTests/testLockout()` | that one test |
| `MyAppUITests/LoginTests/testLockout` | the same test — a trailing `()` is optional on both sides |
| `MyAppUITests/LoginTests/*` | every test in the class |
| `MyAppUITests/*` | every test in the target |
| `*Flaky*` | anything with "Flaky" anywhere — `*` crosses `/` |
| `MyAppUITests/LoginTests/test?()` | one character wildcard |
| `re:MyAppUITests/.*Tests/testLogin.*` | a regular expression, anchored at both ends |

Rules: an empty `include` keeps everything; `include` is applied first and its entries are
OR-ed together; `exclude` is applied afterwards and wins. Tests that the *test plan itself*
disables are never run and are reported separately from the ones your filters dropped.

Flags override the config field they name, not the whole section. `--include` given on the
command line replaces `tests.include` but leaves `tests.exclude` from the file in force.

## Batching strategies

A batch is one `xcodebuild` invocation: a group of tests that will run together, on one
simulator, in one process. The strategy decides which tests end up together.

Set it with `batching.strategy` or `--strategy`.

### `duration` (default)

Balances batches by **predicted running time**, using the durations recorded in
[`.gxcui/timings.json`](#timings). Tests are sorted longest-first and each is dropped into
whichever batch is currently lightest — the standard greedy heuristic for this problem.

Use it unless you have a reason not to. It is the only strategy that stops one slow class
from becoming a straggler that leaves every other simulator idle while it finishes.

With no timing history it treats every test as equal, which makes it behave roughly like
`shard`. It gets better on the second run and stabilises after a few.

### `class`

Keeps every test of a class in the same batch, then balances whole classes across batches
using the same greedy packing.

Use it when a class shares expensive set-up — a `setUp` that logs in, seeds a database or
launches the app in a particular state — so splitting it across simulators would repeat that
work. The trade-off is coarser balancing: one class much larger than the rest becomes a
straggler that no amount of rebalancing can break up.

### `count`

Fixed number of tests per batch (`batching.batchSize`, default 10), in enumeration order.
No balancing at all.

Use it when you want predictable, reproducible batch contents — debugging an ordering
problem, for instance — and do not care about wall-clock time.

### `shard`

Deals tests round-robin across exactly `batching.batches` batches, ignoring durations.

Use it when you have no timing history and want an even *count* per batch, or when you are
deliberately spreading a class's tests as widely as possible to detect inter-test coupling.

### How many batches?

`batching.batches` (or `--batches`) sets the number. **Zero, the default, means two batches
per simulator.**

More batches than simulators is deliberate. With exactly one batch per simulator, a
simulator that finishes early has nothing to do, and the run takes as long as its unluckiest
split. With two, a fast worker takes another batch instead of idling, which absorbs the
error in every duration estimate. More than two adds per-invocation overhead — each
`xcodebuild` launch costs a few seconds — so raise it only if your batches are long enough
for that to be noise.

gxcui never creates more batches than there are tests, and splits any batch that would
exceed 1000 `-only-testing:` arguments, which would otherwise risk the operating system's
argument-length limit.

Batch plans are deterministic: the same tests, options and timings always produce the same
batches in the same order, so `--dry-run` tells you exactly what a real run will do.

## Timings

Every run appends what it measured to `output.timingsFile`, `.gxcui/timings.json` by
default. That file is the input to the `duration` strategy on the next run. Nothing else is
needed to "use old timings" — if the file is there, it is used.

```json
{
  "version": 1,
  "updatedAt": "2026-08-17T13:57:11+03:00",
  "tests": {
    "MyAppUITests/LoginTests/testLockout()": {
      "seconds": 12.43,
      "samples": 7,
      "updatedAt": "2026-08-17T13:57:11+03:00"
    }
  }
}
```

- **Durations come from the result bundle's `durationInSeconds`**, an exact float. gxcui
  never parses the human-readable `duration` string that sits next to it, which is
  locale-formatted (`"0,24s"` on a machine with a comma decimal separator).
- **Each new measurement is folded in as a moving average** weighted 30% to the newest run,
  so one unusually slow run does not dominate the estimate while genuine drift is still
  tracked. `samples` records how many runs contributed.
- **Tests with no history are estimated at the median** of everything known, not at zero.
  That keeps every unknown test from landing in the same batch on the first run after
  someone adds a class.
- **A missing or corrupt file is not an error.** It is treated as an empty cache, and the
  run proceeds with equal weights.

### Sharing timings across machines

The file is written with sorted keys and stable formatting, so it diffs cleanly. Two ways to
use that:

- **Commit it.** Every developer and every CI machine starts with good estimates, including
  the very first run on a fresh checkout. The cost is a file that changes on most runs.
- **Cache it in CI.** Keep `.gxcui/timings.json` in your pipeline's cache, keyed however you
  like. No commits, but a cold cache means one poorly balanced run.

To reset the history, delete the file. To turn the feature off entirely, set
`output.timingsFile: ""` — the `duration` strategy then behaves like an unweighted split.

## Retries and flakiness

```yaml
retries:
  maxAttempts: 2   # total runs per test; 1 means no retries
  isolate: true    # re-run each failed test on its own
```

After the main pass, gxcui collects every test that failed or never reported, and runs them
again — by default **one test per batch**, so a test that only fails alongside a particular
neighbour gets a fair second chance. That repeats until every test passes or `maxAttempts`
is reached.

A test that fails and then passes is **counted as passed and reported as flaky**, because
"passed on the third try" is not the same news as "passed". A test that ran three times and
failed all three is not flaky, it is broken, and is reported as a plain failure.

Every attempt is recorded in `run.json` with its batch, device, result, duration and failure
messages, so you can see exactly what happened rather than just the final verdict.

gxcui does its own retrying rather than using xcodebuild's `-retry-tests-on-failure`, which
hides the attempts inside a single result bundle and takes scheduling away from gxcui.

### When a batch dies

The exit code tells gxcui where to look; the result bundle tells it what happened. Only exit
65 means "the tests ran and some failed" — every other non-zero code needs interpreting.

So after each batch gxcui compares the tests it *asked* to run against the tests that
*appear* in the result bundle. Anything missing is reported as **unaccounted**, not silently
dropped, and is eligible for a retry. That is what a wedged simulator, a crashed test host
or a batch timeout looks like from the outside, and it is the failure mode most likely to
lose tests quietly.

Unaccounted tests appear in their own section of the summary, count against
`summary.unaccounted` in `run.json`, and make the run fail.

## Output

Every run writes a timestamped directory under `output.dir`:

```
.gxcui/runs/20260817-135615/
├── batches/
│   ├── batch-01.xcresult                 one result bundle per batch invocation
│   └── batch-02.xcresult
├── logs/
│   ├── batch-01.log                      full xcodebuild output for that batch
│   └── batch-02.log
├── merged.xcresult                       every batch combined into one bundle
├── junit.xml
├── report.html                           self-contained HTML report
└── run.json                              the full record of the run
```

Batch names are sequential — `batch-03`, not a UUID — so a failure in the summary, its log
file and its result bundle are obviously the same thing, and so batch 3 of today's run is
comparable with batch 3 of yesterday's. The number is padded so a directory listing stays in
plan order. The name says nothing about the contents on purpose: only the `class` strategy
puts a single class in a batch, so naming a batch after a class it holds would be a promise
the other three strategies break. What a batch actually holds is listed in `run.json`, and up
front by `--dry-run`. `run.json` additionally records a content hash per batch, which
identifies the same *set of tests* across runs even when the numbering shifts.

Set `output.keepResultBundles: false` to delete the per-batch bundles once they have been
merged, if disk space matters more than per-batch detail.

### `run.json`

The machine-readable record of everything that happened. Useful fields:

| Field | What it holds |
|---|---|
| `summary` | totals: passed, failed, skipped, flaky, unaccounted |
| `devices` | the simulators used |
| `batches[]` | per invocation: id, hash, attempt, device, tests, status, exit code, the exact command, timings, log and bundle paths |
| `batches[].status` | `completed`, `no-results`, `timed-out` or `cancelled` |
| `tests[]` | per test: final result, every attempt with its device and failure messages, flaky flag |
| `artifacts` | where the merged bundle, JUnit report and logs were written |
| `interrupted` | true if the run was cancelled |

### Interrupting a run

`Ctrl-C` stops scheduling new batches and kills the ones in flight, then still merges what
finished and writes all the reports. A run cancelled at minute 38 of 40 should not throw
away 38 minutes of work. The result is marked `interrupted` and exits non-zero.

## Reports

### JUnit

`junit.xml` is written by default. One `<testsuite>` per class, named `Target.Class`, which
is what CI servers group on. Durations are plain decimal seconds, never locale-formatted.
The simulator a suite ran on is recorded as a `gxcui.devices` property — and as the suite's
`hostname` when the whole suite ran on one — since a run is spread across several.

Retried tests carry a `<system-out>` note — `gxcui: ran 2 times (flaky: passed after
failing)` — rather than a non-standard element, so strict parsers still accept the document.

### HTML

`report.html` is written by default, from the merged bundle. It is a single file: the
stylesheet, the screenshots and the screen recordings are all inlined, so it can be
archived by CI, emailed, or attached to a bug without a directory of assets beside it.

It shows the run's totals and the simulators it used, then a collapsible tree of
bundle → class → test. Failed suites and classes are expanded on load;
everything else starts collapsed. There is a filter box and a "failures only" toggle at the
top. Each failing test shows its assertion message and source location, and — where they
were collected — its step-by-step activity log with the screenshots and recordings captured
along the way.

Some of the report comes from gxcui rather than from the bundle, because a result bundle
has no concept of it: a test that was retried is badged with its attempt count, one that
passed only after failing is badged `flaky`, and the run's duration is gxcui's own.

The header shows two durations:

```
⏱ 23m 22s elapsed        🧪 1h 23m of tests across 4 simulators
```

Elapsed is the wall-clock time the run took. The second is every test's duration added up —
the work that would have taken that long on one simulator. The gap between them is what the
parallelism bought, and it is only shown when there is more than one simulator to report.

#### It reads the per-batch bundles, not the merge

The report is built from `batches/*.xcresult` directly rather than from `merged.xcresult`,
because merging loses information that the report wants:

- **`xcresulttool merge` does not union its inputs' time windows.** It keeps the first
  input's. On a real 8-batch run across 4 simulators — 16:12:41 to 16:36:03, 1402 s — the
  merged bundle claimed 16:13:13 to 16:23:50, which is 637 s and exactly batch 1's window.
  Every batch after the first wave was missing from the run's own duration.
- **Merging replaces simulator names with the device model.** In a merged bundle every one
  of those tests was labelled `iPhone SE (3rd generation)`; read as they are, the batches
  still say `xcpool-1` through `xcpool-4`, so each test says which simulator ran it.

Each per-batch bundle, by contrast, knows its own window, its own simulator and its own
results, so combining them in Go reconstructs the run accurately. Classes split across
several batches are folded back together, and each test is counted once, by its last
result — so the headline counts agree with the tree underneath them even when retries put
the same test in two bundles.

Merging is still on by default, because a single `.xcresult` is what Xcode and CI archiving
want. The report simply no longer depends on it, and is produced even with
`output.merge: false`.

#### Size and speed

Activity logs and attachments each cost one `xcresulttool` call per test, and attachments
are what make the file large — a full UI test run with every screen recording embedded can
reach hundreds of megabytes. So both default to failures only, which is where they are
actually read:

```yaml
output:
  html:
    activities: failed     # none | failed | all
    attachments: failed    # none | failed | all
    maxAttachmentSizeMB: 0 # skip anything larger; 0 means no limit
```

`attachments: failed` means everything a failing test captured — its screen recording as
well as its screenshots — and not just the attachments Xcode itself tags as belonging to a
failure, which are the final UI snapshot and element dump and never the recording.

`activities: none, attachments: none` makes the report nearly free to produce — it is then
two `xcresulttool` calls in total, regardless of how many tests ran.

#### Code coverage

With `--coverage`, or `output.html.coverage: true`, the report gains a collapsible
breakdown of line coverage per target and per source file:

```yaml
output:
  html:
    coverage: true
```

```
Code coverage                                        63.75%   40059 / 62838 lines
  Workforce.app                     ██████░░░  68.56%   26320 / 38392
  WorkforceUITests.xctest           ████████░  88.21%   13015 / 14754
  SBTUITestTunnelServer             █░░░░░░░░  10.96%     703 / 6414
```

It is off by default, for two reasons: coverage is only in the bundles when the run
gathered it, and reading it costs a pass over every batch bundle — about four seconds for
an eight-batch run, and around 200 KB of report.

A few things worth knowing:

- **Batches are unioned, not added.** Each batch only covers the lines its own tests
  reached, and batches overlap heavily. Summing them double-counts: the eight batches above
  add up to 375% of the app's executable lines. gxcui exports each bundle's coverage and
  unions them with `xccov merge`, which gives exactly the same figures as merging the
  bundles first — verified against `merged.xcresult` — without merging gigabytes of
  attachments to reach a few megabytes of coverage.
- **It works on a folder of bundles.** `gxcui report ./ci-artifacts/results --coverage`
  unions whatever is in there. A single bundle is read directly, with no merge step.
- **Bundles without coverage are skipped**, and when none of them have any the section is
  simply left out. Running without coverage gathering is normal, not an error.

  Nothing here reads your scheme — `gxcui report` is given bundles and may have no project
  at all. The bundle records whether coverage was gathered into it, and
  `xcresulttool get content-availability` is what asks:

  ```
  $ xcrun xcresulttool get content-availability --path Results.xcresult
  { "hasCoverage" : false, "hasDiagnostics" : true, "hasTestResults" : true }
  ```

  Every bundle is checked before it is read. That is not a nicety: `xccov` fails outright
  with "No coverage data in result bundle" when pointed at one that has none, so without
  the check a single coverage-free bundle in a folder would take the whole report down
  with it.
- **Files are ranked by uncovered lines**, not by percentage, because the question a
  coverage table gets opened for is where the untested code is. A one-line file at 0% is
  not the place to start.
- Targets with nothing executable in them — resource bundles, header-only dependencies —
  are dropped rather than listed as `0.00% (0/0)`.

Only line coverage is reported. Per-function figures and line-by-line source highlighting
are what Xcode is for; this is the view that answers "what did the suite never touch".

#### Reporting on bundles you already have

`gxcui report` needs nothing from gxcui — no `run.json`, no config, no project. It takes
one `.xcresult`, a directory full of them, or a gxcui run directory:

```bash
# one bundle, from anywhere
gxcui report SomeoneElsesResults.xcresult -o report.html --attachments all

# a directory of bundles, combined into one report
gxcui report ./ci-artifacts/results

# a gxcui run directory
gxcui report .gxcui/runs/20260817-135615 --activities all
```

Given a directory, it reports on every `.xcresult` inside, in name order. That is how a
report covering several simulators is produced without merging anything first — and it
works just as well for bundles from an entirely different tool, as long as `xcresulttool`
can read them. With no `run.json` to consult, the run's window is taken from the earliest
and latest bundle, which is the time actually spent running tests.

A gxcui run directory gets two extras: its `batches/` bundles are preferred over
`merged.xcresult`, and `run.json` beside them supplies the retry and flakiness badges. If
the batch bundles were cleaned up (`keepResultBundles: false`), it falls back to the merged
one.

This is the command to reach for when a run was configured for speed and you now want the
detail — nothing has to be re-run, since it all came out of the bundles.

### Turning reports off

`output.merge: false`, `output.junit: false` and `output.html.enabled: false` each turn one
off. `gxcui run --no-html` skips the HTML report for one run — it is the slowest of the
three to produce — and `gxcui run --no-report` skips all of them.

## Configuration reference

Settings come from `gxcui.yaml` in the working directory, from `--config <path>`, or
entirely from flags. **Flags always win over the file.** Unknown keys are an error, not a
silently ignored setting, so a typo fails loudly.

See [gxcui.example.yaml](gxcui.example.yaml) for the same list as an annotated file.

### `project` — what to test

Set **exactly one** of `workspace`, `project`, `xctestrun` or `testProducts`.

| Key | Default | Meaning |
|---|---|---|
| `workspace` | — | path to an `.xcworkspace`; requires `scheme` |
| `project` | — | path to an `.xcodeproj`; requires `scheme` |
| `xctestrun` | — | a prebuilt `.xctestrun`; skips the build entirely |
| `testProducts` | — | a prebuilt `.xctestproducts` archive |
| `scheme` | — | required with `workspace` or `project`, rejected with the prebuilt inputs |
| `testPlan` | — | test plan name without the `.xctestplan` extension |
| `configuration` | — | build configuration, e.g. `Debug` |
| `derivedDataPath` | `.gxcui/dd` when `run` builds | where the build writes, and where the `.xctestrun` is found |

A build emits one `.xctestrun` per test plan. When there is more than one, gxcui uses
`testPlan` to choose and **errors rather than guessing** if that still leaves it ambiguous —
guessing would silently run the wrong set of tests.

### `simulators` — where to run

| Key | Default | Meaning |
|---|---|---|
| `include` | `[]` | UDIDs or device names to use; empty means every booted simulator |
| `exclude` | `[]` | UDIDs or device names to skip; wins over `include` |
| `bootSims` | `false` | boot the `include` simulators before the run; requires `include` |
| `bootTimeout` | `5m` | how long one simulator gets to boot before the run gives up on it |
| `resetBefore` | `false` | shut down and erase them before the run; requires `bootSims` to boot them back up |
| `shutdownAfter` | `false` | shut them down once the last batch has finished |

See [Booting simulators](#booting-simulators) and
[Erasing and shutting down](#erasing-and-shutting-down). Both apply to the `include`
simulators; `shutdownAfter` with an empty `include` applies to **every** simulator on the
machine.

### `tests` — what to run

| Key | Default | Meaning |
|---|---|---|
| `include` | `[]` | keep only matching tests; empty keeps everything |
| `exclude` | `[]` | drop matching tests; applied after `include` |
| `enumerate.timeout` | `10m` | bound on the discovery step |

Discovery installs and launches the test host to ask it what it contains, so a large UI test
target needs considerably longer than a small one. Raise the timeout if discovery is what
times out.

### `batching` — how to split

| Key | Default | Meaning |
|---|---|---|
| `strategy` | `duration` | `duration`, `class`, `count` or `shard` — see [Batching strategies](#batching-strategies) |
| `batches` | `0` | number of batches; zero means two per simulator |
| `batchSize` | `10` | tests per batch, used only by `count` |

### `execution` — how each batch runs

| Key | Default | Meaning |
|---|---|---|
| `batchTimeout` | `30m` | a batch that overruns is killed; its tests become unaccounted and can be retried |
| `testTimeout` | `0` | seconds any single test may run; zero leaves the test plan's own setting alone |
| `extraArgs` | `[]` | appended verbatim to every `xcodebuild test` invocation |
| `buildArgs` | `[]` | appended verbatim to `build-for-testing`, for things like `CODE_SIGNING_ALLOWED=NO` |

### `retries`

| Key | Default | Meaning |
|---|---|---|
| `maxAttempts` | `1` | total runs per test; one means no retries |
| `isolate` | `true` | re-run each failed test in a batch of its own |

### `output`

| Key | Default | Meaning |
|---|---|---|
| `dir` | `.gxcui/runs` | parent of the per-run directories |
| `merge` | `true` | combine the per-batch bundles into `merged.xcresult` |
| `junit` | `true` | write `junit.xml` |
| `keepResultBundles` | `true` | keep the per-batch bundles after merging |
| `timingsFile` | `.gxcui/timings.json` | per-test durations; empty disables the feature |

#### `output.html`

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `true` | write the HTML report |
| `path` | `report.html` | where to write it, relative to the run directory |
| `activities` | `failed` | include step-by-step logs for `none`, `failed` or `all` tests |
| `attachments` | `failed` | embed screenshots and recordings for `none`, `failed` or `all` |
| `maxAttachmentSizeMB` | `0` | skip any attachment larger than this; `0` means no limit |
| `coverage` | `false` | add the line-coverage breakdown, when the bundles hold any |

The report is built from the per-batch bundles, so it does not need `merge: true`.

Durations accept a Go duration string (`90s`, `5m`, `1h30m`) or a plain number of seconds.

## Command reference

Every command accepts the global flags, which override the corresponding config keys:
`--config`, `--workspace`, `--project`, `--xctestrun`, `--test-products`, `--scheme`,
`--test-plan`, `--configuration`, `--derived-data-path`, `--include`, `--exclude`,
`--simulator`. The pattern and simulator flags are repeatable.

### `gxcui run`

Builds if needed, discovers, batches, runs, retries and reports.

| Flag | Meaning |
|---|---|
| `--dry-run` | print the batch plan and each batch's exact command; builds and enumerates, runs no tests |
| `--strategy` | override `batching.strategy` |
| `--batches N` | override `batching.batches` |
| `--attempts N` | override `retries.maxAttempts` |
| `--output-dir` | override `output.dir` |
| `--boot-sims` | override `simulators.bootSims`; boot the named simulators before running |
| `--reset-before` | override `simulators.resetBefore`; shut down and erase them first, with `--boot-sims` |
| `--shutdown-after` | override `simulators.shutdownAfter`; shut them down when the last batch finishes |
| `--coverage` | override `output.html.coverage`; include code coverage in the report |
| `--no-html` | skip the HTML report, the slowest of the three to produce |
| `--no-report` | skip merging, JUnit and HTML for this run |
| `-q, --quiet` | print only the final summary |

**Exit codes** — distinct on purpose, because CI cannot otherwise tell "your tests are
broken" from "gxcui could not run them":

| Code | Meaning |
|---|---|
| `0` | every test passed |
| `1` | the run completed with failing, unaccounted or interrupted tests |
| `2` | gxcui could not complete the run: bad configuration, no booted simulators, build failure |

### `gxcui enumerate`

Lists the tests that would run, after filtering. Runs on one simulator and does not execute
any tests.

| Flag | Meaning |
|---|---|
| `-f, --format` | `list` (default), `json` or `tree` |
| `--device` | simulator to enumerate on, by UDID or name |
| `-v, --verbose` | also report filtered and disabled tests |
| `--dry-run` | print the xcodebuild command instead of running it |

### `gxcui devices`

Lists the simulators gxcui would use.

| Flag | Meaning |
|---|---|
| `-f, --format` | `list` (default) or `json` |
| `-a, --all` | also show skipped simulators and the reason for each |

### `gxcui report <path>`

Renders result bundles as a single HTML file. The path is one `.xcresult`, a directory of
them (all combined into one report), or a gxcui run directory. `gxcui run` does this
already; use the command for bundles produced elsewhere, or to re-render with more detail
than the run collected. It needs no config and no `run.json`.

| Flag | Meaning |
|---|---|
| `-o, --output` | where to write it (default: `report.html` beside the bundle) |
| `--activities` | step-by-step logs for `none`, `failed` (default) or `all` |
| `--attachments` | screenshots and recordings for `none`, `failed` (default) or `all` |
| `--max-attachment-size N` | skip attachments larger than N MB; `0` means no limit |
| `--coverage` | include code coverage; unioned across every bundle given |
| `--title` | report title (default: the run title recorded in the bundle) |

## CI

Build in one stage and run in another, so the run stage does no compiling:

```bash
# build stage
xcodebuild build-for-testing \
  -workspace MyApp.xcworkspace -scheme MyAppUITests \
  -destination 'platform=iOS Simulator,id=<udid>' \
  -derivedDataPath .gxcui/dd

# test stage
xcrun simctl boot <udid-1>
xcrun simctl boot <udid-2>
gxcui run --xctestrun .gxcui/dd/Build/Products/MyApp_MyAppUITests_iphonesimulator26.5-arm64.xctestrun
```

Collect `.gxcui/runs/<id>/junit.xml` as your test report, `report.html` as a browsable one,
and `merged.xcresult` as an artifact. The HTML report is one self-contained file, so it
needs no special handling from whatever serves your build artifacts. Cache or commit
`.gxcui/timings.json` so batches stay balanced — see [Timings](#timings).

Branch on the exit code: `1` means report the test failures, `2` means the infrastructure
needs attention.

## Troubleshooting

**"no booted simulators"** — gxcui does not boot simulators. Run `xcrun simctl boot <udid>`,
then `gxcui devices` to confirm.

**"no eligible simulator: N booted simulator(s) were filtered out"** — `simulators.include`
or `exclude` excluded everything that is running. `gxcui devices --all` shows the reason per
device.

**"no tests to run: tests.include/exclude dropped all N of them"** — your filters match
nothing. `gxcui enumerate --verbose` lists what was dropped and why. Remember that flags
override only the field they name, so `--include` still leaves `tests.exclude` in force.

**"find .xctestrun: N candidates"** — the build produced one `.xctestrun` per test plan. Set
`project.testPlan` to pick one.

**A batch reports "no-results"** — xcodebuild exited without leaving a readable result
bundle. Its tests are reported as unaccounted and retried. The full output is in
`logs/<batch-id>.log`; the exact command that produced it is in `run.json`.

**Everything is slower than expected** — check the per-batch durations in the summary. If
one batch dominates, you are looking at a straggler: the `duration` strategy needs a run or
two of history, and `--batches` above the default gives the scheduler more to work with.

## Development

Everything that shells out goes through `internal/exec.Runner`, an interface with a real
implementation and a scripted fake, so almost the entire test suite runs without Xcode
installed and without touching a simulator:

```bash
go test ./...
go vet ./...
```

The parsers are tested against real captured output in `docs/research/samples/` — the
enumeration JSON, the result-bundle JSON in both its single-device and merged shapes, and
the schema `xcresulttool` publishes for itself.

```
cmd/gxcui/            CLI: root, devices, enumerate, run, report, progress rendering
executor/             discovery, filtering, batching, scheduling, retries (public API)
reporter/             xcresult parsing, merging, JUnit, HTML, timings (public API)
reporter/templates/   the HTML report's template and stylesheet, embedded at build time
internal/xcodebuild/  argument construction, exit-code semantics, output parsing
internal/simctl/      simulator inventory
internal/exec/        the process seam — real and fake runners
docs/PLAN.md          design, verified xcodebuild behaviour, milestones
docs/research/        real xcodebuild/xcresulttool output the parsers are built against
```

`executor` and `reporter` are usable as libraries: build an `executor.Config` in code and
call `executor.New(cfg).Run(ctx, opts)`, or point `reporter` at result bundles you produced
some other way.

## License

MIT — see [LICENSE](LICENSE).
