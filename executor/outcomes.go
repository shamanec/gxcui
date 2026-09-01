package executor

import (
	"sort"
	"strings"

	"github.com/shamanec/gxcui/reporter"
)

// outcomeSet accumulates what happened to each test across attempts.
type outcomeSet struct {
	order    []string
	outcomes map[string]*TestOutcome
}

func newOutcomeSet() *outcomeSet {
	return &outcomeSet{outcomes: map[string]*TestOutcome{}}
}

// record folds one batch result into the set.
//
// Tests the batch was asked to run but never reported on are recorded with an
// unknown result rather than dropped: a test that vanished because its
// simulator died must stay visible, and is eligible for a retry.
func (s *outcomeSet) record(br BatchResult) {
	for _, c := range br.cases {
		s.append(c.Identifier, TestAttempt{
			Attempt:  br.Attempt,
			Batch:    br.ID,
			Device:   deviceLabel(br, c),
			Result:   c.Result,
			Seconds:  c.Duration,
			Failures: failureMessages(c),
		})
	}
	for _, id := range br.Unaccounted {
		s.append(id, TestAttempt{
			Attempt: br.Attempt,
			Batch:   br.ID,
			Device:  br.Device.Name,
			Result:  reporter.ResultUnknown,
		})
	}
}

// deviceLabel prefers the device the result bundle recorded, falling back to the
// simulator the batch was scheduled on.
func deviceLabel(br BatchResult, c reporter.TestCase) string {
	if c.Device != "" {
		return c.Device
	}
	return br.Device.Name
}

func failureMessages(c reporter.TestCase) []string {
	var msgs []string
	for _, f := range c.Failures {
		switch {
		case f.Message != "":
			msgs = append(msgs, f.Message)
		case f.SourceCode != "":
			msgs = append(msgs, f.SourceCode)
		}
	}
	return msgs
}

func (s *outcomeSet) append(identifier string, attempt TestAttempt) {
	outcome, ok := s.outcomes[identifier]
	if !ok {
		outcome = &TestOutcome{Identifier: identifier}
		s.outcomes[identifier] = outcome
		s.order = append(s.order, identifier)
	}
	outcome.Attempts = append(outcome.Attempts, attempt)
}

// retryable returns the tests whose latest attempt did not pass, which is what
// the next attempt should re-run.
func (s *outcomeSet) retryable() []string {
	var out []string
	for _, id := range s.order {
		outcome := s.outcomes[id]
		last := outcome.LastAttempt()
		if last.Result.Failed() || last.Result == reporter.ResultUnknown {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// finish resolves each test to its final state.
//
// The last attempt wins, so a test that failed and then passed counts as passed
// — but it is marked flaky, because "passed on the third try" is not the same
// news as "passed".
func (s *outcomeSet) finish() []TestOutcome {
	out := make([]TestOutcome, 0, len(s.order))
	for _, id := range s.order {
		outcome := *s.outcomes[id]
		last := outcome.LastAttempt()
		outcome.Result = last.Result
		outcome.Seconds = last.Seconds

		if last.Result.Passed() {
			for _, attempt := range outcome.Attempts[:len(outcome.Attempts)-1] {
				if !attempt.Result.Passed() {
					outcome.Flaky = true
					break
				}
			}
		}
		out = append(out, outcome)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identifier < out[j].Identifier })
	return out
}

// attemptCounts maps each test to how many times it ran, for the JUnit writer.
func attemptCounts(tests []TestOutcome) map[string]int {
	counts := make(map[string]int, len(tests))
	for _, t := range tests {
		counts[t.Identifier] = len(t.Attempts)
	}
	return counts
}

// flakyTests marks the tests that passed only after failing.
func flakyTests(tests []TestOutcome) map[string]bool {
	flaky := map[string]bool{}
	for _, t := range tests {
		if t.Flaky {
			flaky[t.Identifier] = true
		}
	}
	return flaky
}

// finalCases renders the run's final outcomes as report input.
//
// Only the last attempt of each test is included, so a report shows what is true
// now rather than every intermediate failure; the full history stays in the run
// manifest.
func finalCases(tests []TestOutcome, byIdentifier map[string]reporter.TestCase) []reporter.TestCase {
	cases := make([]reporter.TestCase, 0, len(tests))
	for _, t := range tests {
		last := t.LastAttempt()

		c, ok := byIdentifier[t.Identifier]
		if !ok {
			// A test that never reported still belongs in the report, so that
			// "we do not know what happened to this" is visible rather than
			// silently missing.
			c = reporter.TestCase{Identifier: t.Identifier}
			c.Target, c.Suite, c.Name = splitIdentifier(t.Identifier)
		}
		c.Result = t.Result
		c.Duration = t.Seconds
		if last.Device != "" {
			c.Device = last.Device
		}
		if len(last.Failures) > 0 && len(c.Failures) == 0 {
			for _, msg := range last.Failures {
				c.Failures = append(c.Failures, reporter.Failure{Message: msg})
			}
		}
		cases = append(cases, c)
	}
	return cases
}

// splitIdentifier breaks "Target/Class/method()" into its parts. Identifiers
// with more levels put the extra ones in the suite.
func splitIdentifier(identifier string) (target, suite, name string) {
	parts := strings.Split(identifier, "/")
	switch len(parts) {
	case 0:
		return "", "", identifier
	case 1:
		return "", "", parts[0]
	case 2:
		return parts[0], "", parts[1]
	}
	return parts[0], strings.Join(parts[1:len(parts)-1], "/"), parts[len(parts)-1]
}
