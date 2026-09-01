# Research samples

Real output captured on Xcode 26.6 (17F113), `xcresulttool` 24757 / schema 0.1.0, from a
throwaway SwiftPM package with two XCTest classes (one test deliberately failing) run on
two booted iOS 26.5 simulators.

These are meant to become test fixtures — move them into `executor/testdata/` and
`reporter/testdata/` as those packages are written.

| File | Command |
|---|---|
| `enum-flat.json` | `xcodebuild test-without-building -xctestrun … -enumerate-tests -test-enumeration-style flat -test-enumeration-format json` |
| `enum-hier.json` | same, `-test-enumeration-style hierarchical` |
| `tests-schema.json` | `xcrun xcresulttool get test-results tests --schema` |
| `xcresult-tests-single.json` | `xcrun xcresulttool get test-results tests --path <one-batch>.xcresult` |
| `xcresult-tests-merged.json` | same, on a bundle produced by `xcresulttool merge` of two batches — note the extra `Device` level under each `Test Case` |
| `xcresult-summary-merged.json` | `xcrun xcresulttool get test-results summary --path <merged>.xcresult` |

See `docs/PLAN.md` §5 and §9 for what each of these implies for the implementation.
