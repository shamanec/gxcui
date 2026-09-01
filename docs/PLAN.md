# gxcui — Implementation Plan

Parallel XCUITest runner: enumerate tests, split them into batches, run each batch on a
different booted simulator via `xcodebuild`, then report.

Everything in the "Verified behaviour" sections below was confirmed empirically on this
machine (Xcode 26.6, build 17F113, `xcresulttool` 24757 / schema 0.1.0). Captured samples
live in `docs/research/samples/` and should become test fixtures.

---

## 1. Scope

In scope:

- Test enumeration from a project/workspace+scheme, an `.xctestrun`, or a test plan.
- Batching/sharding of the enumerated test suite.
- Parallel execution — one batch at a time per booted simulator, until the queue drains.
- Retry policy for failed tests and for infrastructure failures.
- JUnit XML report + a machine-readable JSON run summary.
- YAML configuration for all of the above.
- HTML reporting. Originally out of scope, since there was already an external
  xcresult→HTML tool; it was folded in once it became clear that a run should not finish by
  telling you to go and run something else, and that gxcui knows two things the tool could
  not — which simulator ran a test, and how many attempts it took.

Out of scope (for now):

- Booting/creating/cloning simulators. gxcui consumes *already booted* simulators. (A
  `gxcui devices` command that only *reports* health is worth having; provisioning is not.)
- Physical devices, macOS/tvOS/watchOS targets. The design shouldn't preclude them, but
  don't build for them.

---

## 2. Package layout

Two public packages, as requested, plus internal plumbing:

```
cmd/gxcui/                CLI entry point (cobra): run, enumerate, devices, report, version
executor/                 PUBLIC: the orchestration API
  config.go               executor.Config (populated from YAML or by a library caller)
  plan.go                 input resolution: project|workspace|xctestrun|testplan -> RunPlan
  enumerate.go            test discovery -> TestID list
  batch.go                batching strategies
  schedule.go             worker-per-simulator scheduler, retries, cancellation
  run.go                  Execute(ctx, Config) (*Result, error)
reporter/                 PUBLIC: xcresult -> model -> report writers
  model.go                normalized run model (suites, cases, attempts, devices)
  xcresult.go             parse `xcresulttool get test-results tests|summary`
  merge.go                wrapper over `xcresulttool merge`
  junit.go                JUnit XML writer
  details.go              summary, test-details, activities, attachment export
  html.go                 report model + self-contained HTML writer
  templates/              the HTML template and stylesheet, embedded at build time
  timings.go              per-test durations, persisted for the next run's batching
internal/xcodebuild/      argv construction + process execution + exit-code semantics
internal/simctl/          `simctl list devices -j`, boot status, health checks
internal/plist/           .xctestrun introspection (test targets, platform, blueprint names)
internal/logging/         structured logging, per-batch log files
internal/exec/            Runner interface (real + fake) — the seam that makes this testable
```

Design rule: `executor` and `reporter` never call `exec.Command` directly. They go through
`internal/exec.Runner`, an interface with a real implementation and a scripted fake. That's
what makes the whole thing unit-testable without Xcode.

`executor` and `reporter` are decoupled: the executor's output is a `Result` containing
per-batch `.xcresult` paths + metadata; the reporter consumes exactly that (or a bare list
of bundle paths, so `gxcui report` works standalone on someone else's bundles).

Dependencies to add (keep it small):

- `github.com/spf13/cobra` — subcommands.
- `gopkg.in/yaml.v3` — config. Use `KnownFields(true)` so typos in YAML are errors.
- `howett.net/plist` — read `.xctestrun` (it's a binary/XML plist).
- stdlib `encoding/xml` for JUnit output. No dependency needed.

Note: local toolchain is go1.21.6. Bump `go.mod`/toolchain before starting if you want
newer stdlib (`slices`, `log/slog` improvements, `testing/synctest`).

---

## 3. Execution model

```
resolve inputs ─► build-for-testing (optional) ─► locate .xctestrun ─► enumerate
                                                                          │
                          ┌───────────────────────────────────────────────┘
                          ▼
                 filter ─► batch ─► queue
                                     │
        ┌────────────────────────────┼────────────────────────────┐
        ▼                            ▼                            ▼
   worker: sim A               worker: sim B               worker: sim C
   test-without-building       test-without-building       test-without-building
   -only-testing:...           -only-testing:...           -only-testing:...
   -resultBundlePath b1        -resultBundlePath b2        -resultBundlePath b3
        └────────────────────────────┼────────────────────────────┘
                                     ▼
                        collect ─► retry pass(es) ─► merge ─► report
```

The pivot of the whole design: **build once, run many**. `build-for-testing` produces an
`.xctestrun`; every batch then uses `test-without-building -xctestrun`, which does zero
compilation and can safely run N times concurrently. Running batches via
`-project/-scheme` instead would make every worker contend on the same build graph.

### Verified behaviour

- `xcodebuild build-for-testing -scheme X -destination 'platform=iOS Simulator,id=UDID'
  -derivedDataPath ./DD` writes `DD/Build/Products/<Scheme>_<Plan>_<sdk>-<arch>.xctestrun`.
- Two `test-without-building -xctestrun` processes against the *same* xctestrun, on two
  different booted simulators, ran concurrently and produced independent result bundles.
- `-only-testing:` accepts identifiers with **or without** the trailing `()`.
- Exit code was `0` for the passing batch and `65` for the batch containing a failure.

---

## 4. Input resolution

The user may point gxcui at any of these; resolve each to an `.xctestrun` + destination set.

| Input | Handling |
|---|---|
| `.xctestrun` | Use directly. No build. Fastest path, and the one CI should prefer. |
| `.xcodeproj` / `.xcworkspace` + scheme | `build-for-testing` into a gxcui-owned derived-data dir, then discover the emitted `.xctestrun`. |
| `.xctestplan` | A test plan is not independently runnable — it needs a scheme. Resolve to project/workspace + scheme + `-testPlan <name>` (name = filename without extension), then as above. Use `xcodebuild -showTestPlans` to validate the name and to list plans in `gxcui enumerate`. |
| `.xctestproducts` | `-testProductsPath` works with `test-without-building`. Cheap to support; treat as a variant of the xctestrun path. Low priority. |

Gotchas:

- A build can emit **multiple** `.xctestrun` files (one per test plan). Filter by the
  configured plan name, and hard-error on ambiguity rather than guessing.
- The xctestrun filename encodes SDK and arch (`..._iphonesimulator26.5-arm64.xctestrun`).
  Use that to reject simulators whose runtime doesn't match, with a clear error message.
- Parse the xctestrun plist (`internal/plist`) to extract `TestPlan.Name`,
  `TestConfigurations[].TestTargets[].BlueprintName`, and whether targets are UI tests
  (presence of `UITargetAppPath`). Useful for validation and for `-only-testing` prefixes.
- Add `skipBuild: true` config for CI pipelines that build in a separate stage.

---

## 5. Enumeration

Command, run once per resolved plan against one healthy simulator:

```
xcodebuild test-without-building \
  -xctestrun <path> \
  -destination 'platform=iOS Simulator,id=<UDID>' \
  -enumerate-tests \
  -test-enumeration-style flat \
  -test-enumeration-format json \
  -test-enumeration-output-path <tmpfile>
```

### Verified behaviour

**Use `flat`, not `hierarchical`.** Flat yields exactly the identifiers `-only-testing:`
consumes, plus the disabled set:

```json
{
  "errors": [],
  "values": [
    {
      "testPlan": "Sample-Package",
      "enabledTests":  [ { "identifier": "SampleTests/AlphaTests/testOne()" }, ... ],
      "disabledTests": []
    }
  ]
}
```

Hierarchical (`values[].kind = plan|target|class|test` with `children`) is still worth
parsing — it's the natural source for class-aware batching and for a `gxcui enumerate
--tree` display — but the identifiers must then be reassembled from the path.

Both samples are saved at `docs/research/samples/enum-flat.json` / `enum-hier.json`.

Practical notes:

- **Write to a file, not `-`.** With `-test-enumeration-output-path -`, the JSON is
  interleaved with xcodebuild's banner and `** TEST EXECUTE SUCCEEDED **` on stdout.
  Writing to a temp file and reading it back removes all parsing ambiguity. This is
  internal plumbing only: the file is xcodebuild's output, it is deleted as soon as it has
  been read, and gxcui's own output always goes to stdout.
- `-destination` is mandatory for enumeration, and enumeration actually exercises the
  simulator (~4s for a trivial package; expect meaningfully more for a real UI test host
  that must install and launch the app).
- Enumeration works via `-scheme` too (`xcodebuild test -scheme … -enumerate-tests`), which
  implicitly builds. Support it for `gxcui enumerate` convenience, but the run path should
  always go through the xctestrun.
- `values` is an array — plan for multiple test plans / configurations, don't index `[0]`.
- Check `errors` and fail loudly; an empty `enabledTests` with no errors means the filter
  or test plan is wrong, and silently running zero tests is the worst outcome.
- Cache enumeration results keyed by a hash of (xctestrun mtime+size, test bundle mtimes,
  plan name). Optional, but it removes a fixed ~10–60s from every re-run.
- Swift Testing identifiers (parameterized cases, `Suite/test(arg:)`) come through this
  same channel. Don't assume the `Target/Class/method` shape when parsing — treat the
  identifier as opaque, split only on the first two `/` for grouping.

---

## 6. Filtering and batching

Filtering, applied to the enumerated set in this order: drop `disabledTests` → apply
`include` patterns (empty = all) → apply `exclude` patterns. Support glob and `re:`-prefixed
regex against the full identifier. Fail if the result is empty.

Batching strategies (config: `batching.strategy`):

| Strategy | Behaviour | When |
|---|---|---|
| `class` (default) | Group by `Target/Class`, then bin-pack classes across N bins. | Safest for XCUITest — a class shares `setUp`/app state, and splitting one across simulators multiplies app launches. |
| `count` | Fixed number of tests per batch. | Predictable, ignores structure. |
| `shard` | Exactly N batches, round-robin. | N == simulator count; simplest possible. |
| `duration` | Bin-pack by historical per-test duration from a previous run's timing file. | Best wall-clock. Needs `timings.json` persisted from previous runs (`durationInSeconds` is already in the xcresult data). |

Key parameters: `batching.batchSize`, `batching.maxBatches`, `batching.keepClassesTogether`.

Design detail: batching produces `[]Batch{ID, Tests []TestID}`; the scheduler assigns
batches to simulators. Deliberately do *not* pre-assign batches to devices — a dynamic
work queue self-balances when one simulator is slower.

Guard rail: a batch's `-only-testing` list becomes argv. Thousands of tests in one process
invocation can hit argv limits — cap arguments per invocation (~1000 tests) and split.

---

## 7. Scheduling and execution

- Discover devices: `xcrun simctl list devices -j`, filter `state == "Booted"`, then
  intersect with config allow/deny lists (by UDID or name) and with the runtime implied by
  the xctestrun. Error out with a helpful message if zero remain.
- One goroutine per simulator. Batches come off a shared channel. Worker loops until the
  channel drains or ctx is cancelled.
- **Never two concurrent batches on one simulator.** XCUITest needs exclusive control of
  the device.
- Per-batch invocation:

```
xcodebuild test-without-building \
  -xctestrun <path> \
  -destination 'platform=iOS Simulator,id=<UDID>' \
  -resultBundlePath <out>/batches/<batch-id>.xcresult \
  -derivedDataPath <out>/dd/<worker-id> \
  -parallel-testing-enabled NO \
  -test-timeouts-enabled YES \
  -maximum-test-execution-time-allowance <cfg> \
  -only-testing:A/B/c -only-testing:A/B/d ...
```

  - `-parallel-testing-enabled NO` is important: gxcui *is* the parallelism; letting
    xcodebuild also clone simulators would fight it.
  - A per-worker `-derivedDataPath` isolates concurrent writers. (The shared default
    happened to work in my test, but reports of contention are common enough that
    isolation is cheap insurance.)
  - `-resultBundlePath` must point at a path that does **not** already exist.
- Stream each batch's stdout/stderr to `<out>/logs/<batch-id>.log`; keep the last N KB in
  memory for the failure summary. Optional `xcbeautify`/`xcpretty` passthrough if present.
- Context cancellation must kill the process group (`Setpgid`, negative PID signal) —
  xcodebuild spawns children that outlive a bare SIGKILL to the parent.
- Per-batch wall-clock timeout from config; a timed-out batch is an infra failure (below).

### Failure taxonomy — get this right, it's the core of reliability

| Situation | Signal | Response |
|---|---|---|
| Tests ran, some failed | non-zero exit (65), xcresult exists and lists the tests | Test-level retry (§8) |
| Tests ran, all passed | exit 0, xcresult complete | Done |
| Batch crashed / sim wedged / timeout | missing or partial xcresult, tests unaccounted for | Infra retry: requeue the *unaccounted* tests, preferably on a different simulator; mark the sim suspect after K failures and drop it from the pool |
| Bad invocation (unknown test id, bad xctestrun) | non-zero exit, no xcresult, fails identically everywhere | Fail the run fast — don't burn retries |

Never trust the exit code alone. The authority on what happened is the xcresult; the exit
code only tells you where to start looking. Always diff requested-tests against
tests-present-in-xcresult — that difference is what gets requeued.

---

## 8. Retries

- `retries.maxAttempts` (default 1 = no retry), `retries.strategy: per-test | per-batch`.
- Failed tests from attempt N are re-batched (usually one test per batch) and run in a
  retry pass after the main pass drains, so retries don't starve first-attempt work.
- Track attempts per test ID. Final status = last attempt's result; the report must record
  *all* attempts and flag `flaky` (failed then passed).
- Prefer a different simulator for a retry when one is free.
- Deliberately don't use xcodebuild's own `-retry-tests-on-failure`: it hides the retry
  inside a single result bundle and takes control of scheduling away from gxcui. Mention it
  in docs as an alternative for people who want it, but don't build on it.

---

## 9. Reporter

### Parsing

Primary source: `xcrun xcresulttool get test-results tests --path <bundle> --compact`.
Secondary: `... get test-results summary --path <bundle>` for per-device counts.

The output schema is published by the tool itself
(`xcresulttool get test-results tests --schema`, saved at
`docs/research/samples/tests-schema.json`) — mirror it exactly in Go:

```go
type Tests struct {
    TestPlanConfigurations []Configuration `json:"testPlanConfigurations"`
    Devices                []Device        `json:"devices"`
    TestNodes              []TestNode      `json:"testNodes"`
}

type TestNode struct {
    NodeIdentifier    string     `json:"nodeIdentifier"`
    NodeIdentifierURL string     `json:"nodeIdentifierURL"`
    NodeType          string     `json:"nodeType"`   // Test Plan | Unit test bundle | UI test bundle |
                                                     // Test Suite | Test Case | Device | Failure Message | ...
    Name              string     `json:"name"`
    Details           string     `json:"details"`
    Duration          string     `json:"duration"`
    DurationInSeconds float64    `json:"durationInSeconds"`
    Result            string     `json:"result"`     // Passed | Failed | Skipped | Expected Failure | unknown
    Tags              []string   `json:"tags"`
    Children          []TestNode `json:"children"`
}
```

### Verified gotchas — each of these will bite

1. **`duration` is locale-formatted.** I got `"0,24s"` (comma decimal separator) on this
   machine. Never parse it. Use `durationInSeconds` (float) exclusively.
2. **`nodeIdentifier` on a Test Case omits the target.** It's `BetaTests/testFails()`, not
   `SampleTests/BetaTests/testFails()`. Reconstruct the full identifier by walking down
   from the enclosing `Unit test bundle`/`UI test bundle` node — you need the full form to
   match against `-only-testing` identifiers for retry bookkeeping.
3. **Merged bundles have an extra tree level.** A single-device bundle is
   `Test Plan → bundle → Test Suite → Test Case`. After merging two bundles, each
   `Test Case` gains `Device` children (`nodeIdentifier` = the UDID), and
   `Failure Message` hangs off the `Device` node rather than the `Test Case`. The walker
   must handle both shapes. Both samples are checked in.
4. `nodeType` and `result` are open-ended strings in practice — treat unknown values as
   `unknown` rather than erroring.
5. Failure text lives in the `name` of a `Failure Message` child
   (`"AlphaTests.swift:11: XCTAssertEqual failed: …"`); `Source Code Reference` children
   carry the location. Both are needed for a useful JUnit `<failure>`.

### Merging

`xcrun xcresulttool merge a.xcresult b.xcresult --output-path merged.xcresult` works and is
the cleanest way to hand the HTML reporter a single bundle. Verified: the merged
bundle keeps both devices, unions the test tree, and the summary retains per-device counts.

Do it as a *post-processing convenience* (`report.mergeResultBundles: true`), not as a
prerequisite — the JUnit writer should work by parsing each bundle and combining in Go, so
a failed merge never costs you the report. Note that merge writes format `[v3]`; keep the
tool-version dependency documented.

### Outputs

- **JUnit XML** — `<testsuites>` with one `<testsuite>` per test class:
  `classname="Target.Class"`, `name="testMethod()"`, `time` from `durationInSeconds`,
  `<failure message=… type="XCTAssert">` carrying the failure message(s),
  `<skipped/>` for `Skipped`. Put the device name in `hostname` or a `<property>`; record
  retry attempts as `<flakyFailure>`-style properties or, more portably, keep only the
  final attempt in the XML and put full attempt history in the JSON summary. Validate the
  output against the common Jenkins/JUnit5 schema — CI consumers are unforgiving.
- **JSON summary** — the gxcui-native format: run metadata, per-batch info (device, wall
  time, exit code, log path), per-test attempts, flaky list, totals. This is what future
  tooling (including duration-based batching) reads.
- **HTML report** — one self-contained file built from the merged bundle. Its model
  (`reporter.HTMLReport`) is deliberately separate from both the xcresulttool schema types
  and the template, so neither end has to change when the other does.

  Three things were learned building it:

  - Bundle and suite nodes carry **no duration at all** — only test cases do. Suite and
    class totals have to be summed from the tests below them, or they render blank.
  - `xcresulttool merge` does **not** union its inputs' time windows — it keeps the first
    input's verbatim. Verified on a real run: eight batches over four simulators from
    16:12:41 to 16:36:03 (1402s) produced a merged summary reading 16:13:13→16:23:50
    (637.0s), identical to batch 1's own window to the tenth of a second.
  - Merging also **replaces simulator names with the device model**: a Device node in a
    merged tree is named `iPhone SE (3rd generation)`, not `xcpool-3`, so a merged bundle
    cannot say which simulator ran a test.

  Both of those are why the reporter reads the per-batch bundles directly
  (`BuildHTMLFromBundles`) rather than reporting on the merge. Each bundle carries its own
  correct window, its own simulator and its own results; combining them in Go reconstructs
  the run. Merging stays on by default because a single `.xcresult` is what Xcode and CI
  archiving want, but nothing in the reporting path depends on it any more.

  Counts come from the combined rows rather than any summary: one bundle's summary covers
  only its batch, and a merged bundle counts a retried test once while its tree shows both
  runs. Counting unique identifiers by last result keeps the header consistent with the
  tree below it, and agrees with the executor's own `Summary`.

  gxcui's own clock still wins over anything derived from the bundles when it is available,
  since the bundles only span the first batch to the last and a run also spends time
  building and enumerating first — 1402s versus 1367s on the run above.
  - `duration` is locale-formatted (`"0,0012s"` on this machine). Always read
    `durationInSeconds`, which is present on every node that has a duration.
  - Activity attachments name themselves by UUID, but the file on disk is named by
    `xcresulttool export attachments`. The exported `manifest.json` is the authority;
    matching on the UUID prefix works today and the manifest's per-test list, matched on
    capture timestamp, is the fallback.

  Activity logs and attachments each cost an `xcresulttool` call per test, and attachments
  dominate the file size, so each is a three-way setting (`none`/`failed`/`all`) defaulting
  to failures only. Attachments a test recorded but no fetched activity accounted for are
  attached to the test itself, so turning activity logs off never silently discards
  screenshots.

---

## 10. Configuration

`gxcui.yaml`, loaded with strict field checking; every field overridable by CLI flag.

```yaml
version: 1

project:
  # exactly one of: workspace, project, xctestrun, testProducts
  workspace: MyApp.xcworkspace
  scheme: MyAppUITests
  testPlan: Smoke          # optional; .xctestplan name without extension
  configuration: Debug
  skipBuild: false
  derivedDataPath: .gxcui/dd
  buildSettings:           # passed as SETTING=value
    CODE_SIGNING_ALLOWED: "NO"

simulators:
  source: booted           # booted | list
  include: []              # UDIDs or names; empty = all booted
  exclude: []
  maxParallel: 0           # 0 = one worker per eligible simulator
  requireRuntimeMatch: true

tests:
  include: []              # globs or "re:" regex over Target/Class/method
  exclude:
    - "MyAppUITests/FlakyTests/*"
  enumerate:
    cache: true
    timeout: 5m

batching:
  strategy: class          # class | count | shard | duration
  batchSize: 10
  keepClassesTogether: true
  timingsFile: .gxcui/timings.json

execution:
  batchTimeout: 30m
  testTimeout: 300         # -maximum-test-execution-time-allowance
  extraArgs: []            # escape hatch, appended verbatim
  env: {}                  # TEST_RUNNER_* env passthrough

retries:
  maxAttempts: 2
  strategy: per-test       # per-test | per-batch
  retryOnDifferentSimulator: true
  infraMaxAttempts: 2

output:
  dir: .gxcui/out
  keepResultBundles: true
  mergeResultBundles: true
  reports:
    - junit                # junit | json
  junitPath: .gxcui/out/junit.xml
```

CLI surface:

```
gxcui run [-c gxcui.yaml] [--scheme …] [--only …] [--dry-run]
gxcui enumerate [--tree] [--json]     # list what would run; no execution
gxcui devices                         # booted sims + eligibility/health
gxcui report --bundles <dir> --junit out.xml [--merge merged.xcresult]
gxcui version
```

`--dry-run` (print the resolved batches and the exact xcodebuild argv per batch, run
nothing) is the single most useful debugging feature here. Build it early.

---

## 11. Milestones

Status as of the current build: M1–M12 are done. M11 (duration-based batching) landed ahead
of schedule, since the timings file falls out of the reporter for free, and M12 (HTML
reporting) was added to the plan after the fact. What remains is `--rerun-failed`,
`--shard N/M` and simulator health checks.

| # | Deliverable | Done when |
|---|---|---|
| M1 | `internal/exec` Runner + fake; `internal/simctl` device discovery; `gxcui devices` | `gxcui devices` lists booted sims; simctl parsing unit-tested against a captured JSON fixture |
| M2 | `internal/xcodebuild` argv builder + exit-code semantics; `internal/plist` xctestrun reader | Argv construction is table-tested; xctestrun introspection returns targets/plan/platform |
| M3 | Input resolution + `build-for-testing` + xctestrun discovery | Project/workspace/testplan/xctestrun inputs all resolve to one xctestrun; ambiguity errors are clear |
| M4 | Enumeration (flat + hierarchical) with caching; `gxcui enumerate` | Parses the checked-in samples; prints tree and flat identifier list |
| M5 | Batching strategies + `--dry-run` | Deterministic batches for a fixed input; dry-run prints exact argv |
| M6 | Scheduler + parallel execution + log capture + cancellation | Real run across ≥2 booted sims produces one xcresult per batch; Ctrl-C kills all children |
| M7 | Failure taxonomy + retries + requeue of unaccounted tests | Injected failures (killed sim, bad test id, failing test) each take the right branch |
| M8 | `reporter`: xcresult parsing, normalization, merge | Golden-file tests over single + merged samples produce identical models |
| M9 | JUnit + JSON writers | JUnit validates against the JUnit schema; CI consumes it |
| M10 | YAML config + full CLI wiring + docs | End-to-end run driven purely by `gxcui.yaml` |
| M11 | Duration-based batching using persisted timings | Second run is measurably better balanced than the first |
| M12 | HTML report + `gxcui report` | One self-contained file with failures, activity logs and embedded attachments; renders a bundle gxcui did not produce |

M1–M6 is the walking skeleton and the point at which the tool is already useful.

---

## 12. Testing strategy

- **Unit, no Xcode required** (the bulk): the `Runner` fake returns canned stdout/exit
  codes. Every parser gets golden-file tests against `docs/research/samples/*` (move these
  into `executor/testdata/` and `reporter/testdata/` as the packages appear).
- **Property/table tests** for batching: every input test appears exactly once across all
  batches; batch count and size respect config; class grouping holds.
- **Integration, `//go:build xcode`**: a tiny fixture package (like the one used for this
  research — a SwiftPM package with a couple of XCTest classes, one deliberately failing)
  built and run against booted simulators. Gate it behind a build tag so `go test ./...`
  stays fast and hermetic.
- **CI**: `go vet` + `go test ./...` on Linux for the hermetic tests; a macOS job with
  Xcode for the tagged integration tests.

---

## 13. Risks and open questions

| Risk | Mitigation |
|---|---|
| Simulator flakiness above ~3 concurrent sims (a widely reported failure mode) | Health check before assigning a batch (`simctl bootstatus`); suspect-sim ejection after K infra failures; document a conservative default |
| Enumeration is slow on large UI test targets (must install + launch the host app) | Cache enumeration; allow `tests.include` to short-circuit; allow supplying a pre-computed test list |
| `xcresulttool` output schema shifts between Xcode versions | Pin to schema `0.1.0` via `--schema-version`, assert the version at startup, keep golden fixtures per Xcode version |
| The legacy `xcresulttool get --format json` API is deprecated | Use only the `get test-results …` commands. Never add `--legacy` |
| Locale-dependent output (`"0,24s"`) | Never parse human-readable fields; consider forcing `LC_ALL=C` on child processes anyway |
| argv length limits with huge `-only-testing` lists | Cap tests per invocation and split batches |
| Merge behaviour across many bundles (I verified 2) | Test with 10+; fall back to in-Go combination — the JUnit path must not depend on merge |
| Test host app state leaking between batches on one simulator | Config option to `simctl terminate`/uninstall or erase between batches; off by default (it's slow) |

Open questions worth deciding before M5:

1. Should a batch's `-only-testing` list be capped by count or by predicted duration?
2. Do you want `gxcui` to shut down/erase simulators it didn't boot? (Recommendation: no,
   never touch lifecycle by default.)
3. Multiple test plans in a single run — one xctestrun per plan, sequentially, or fan out?
   Simplest correct answer: one plan per run; document running gxcui twice.

---

## 14. Reference — verified commands

```bash
# Build once
xcodebuild build-for-testing -scheme <Scheme> \
  -destination 'platform=iOS Simulator,id=<UDID>' -derivedDataPath ./DD

# Enumerate (flat identifiers, straight into -only-testing)
xcodebuild test-without-building -xctestrun <path> \
  -destination 'platform=iOS Simulator,id=<UDID>' \
  -enumerate-tests -test-enumeration-style flat \
  -test-enumeration-format json -test-enumeration-output-path /tmp/enum.json

# Run one batch
xcodebuild test-without-building -xctestrun <path> \
  -destination 'platform=iOS Simulator,id=<UDID>' \
  -resultBundlePath out/batch-1 -parallel-testing-enabled NO \
  -only-testing:Target/ClassA -only-testing:Target/ClassB/testC

# Read results
xcrun xcresulttool get test-results tests   --path out/batch-1.xcresult --compact
xcrun xcresulttool get test-results summary --path out/batch-1.xcresult
xcrun xcresulttool get test-results tests   --schema        # authoritative schema

# Merge
xcrun xcresulttool merge out/batch-1.xcresult out/batch-2.xcresult \
  --output-path out/merged.xcresult

# Devices / plans
xcrun simctl list devices -j
xcodebuild -scheme <Scheme> -showTestPlans
```
