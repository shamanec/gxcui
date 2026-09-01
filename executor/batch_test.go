package executor

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

func testIDs(class string, n int) []string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("App/%s/test%02d()", class, i))
	}
	return out
}

func allTests(batches []Batch) []string {
	var out []string
	for _, b := range batches {
		out = append(out, b.Tests...)
	}
	sort.Strings(out)
	return out
}

// Every strategy must place every test exactly once. Losing or duplicating a
// test silently is the worst failure mode a batcher has.
func TestPlanCoversEveryTestExactlyOnce(t *testing.T) {
	tests := append(testIDs("LoginTests", 7), testIDs("CheckoutTests", 5)...)
	tests = append(tests, testIDs("SettingsTests", 3)...)

	for _, strategy := range Strategies {
		t.Run(string(strategy), func(t *testing.T) {
			batches, err := Plan(tests, BatchOptions{Strategy: strategy, Simulators: 3, BatchSize: 4})
			if err != nil {
				t.Fatalf("Plan() error = %v", err)
			}
			if len(batches) == 0 {
				t.Fatal("Plan() produced no batches")
			}

			want := append([]string(nil), tests...)
			sort.Strings(want)
			if got := allTests(batches); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("batched tests do not match the input\n got: %v\nwant: %v", got, want)
			}
		})
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	tests := append(testIDs("LoginTests", 9), testIDs("CheckoutTests", 6)...)
	opts := BatchOptions{Strategy: StrategyDuration, Simulators: 2}

	first, err := Plan(tests, opts)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	second, err := Plan(tests, opts)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}

	for i := range first {
		if first[i].ID != second[i].ID || strings.Join(first[i].Tests, ",") != strings.Join(second[i].Tests, ",") {
			t.Fatalf("batch %d differs between identical plans:\n%v\n%v", i, first[i], second[i])
		}
	}
}

// The default batch count is two per simulator so a fast worker can take extra
// work instead of idling.
func TestPlanDefaultsToTwoBatchesPerSimulator(t *testing.T) {
	batches, err := Plan(testIDs("LoginTests", 40), BatchOptions{Simulators: 3})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(batches) != 6 {
		t.Errorf("got %d batches for 3 simulators, want 6", len(batches))
	}
}

func TestPlanNeverMakesMoreBatchesThanTests(t *testing.T) {
	batches, err := Plan(testIDs("LoginTests", 2), BatchOptions{Simulators: 8})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(batches) != 2 {
		t.Errorf("got %d batches for 2 tests, want 2", len(batches))
	}
	for _, b := range batches {
		if b.Size() == 0 {
			t.Errorf("batch %s is empty", b.ID)
		}
	}
}

// StrategyClass must never split a class across batches.
func TestPlanClassKeepsClassesTogether(t *testing.T) {
	tests := append(testIDs("LoginTests", 5), testIDs("CheckoutTests", 5)...)
	tests = append(tests, testIDs("SettingsTests", 5)...)

	batches, err := Plan(tests, BatchOptions{Strategy: StrategyClass, Batches: 3})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}

	for _, b := range batches {
		classes := map[string]bool{}
		for _, test := range b.Tests {
			classes[classOf(test)] = true
		}
		if len(classes) > 1 {
			t.Errorf("batch %s mixes classes %v", b.ID, classes)
		}
	}
}

// Duration balancing should put the one very slow test on its own and group the
// fast ones, rather than splitting purely by count.
func TestPlanDurationBalancesByEstimate(t *testing.T) {
	tests := []string{
		"App/SlowTests/testSlow()",
		"App/FastTests/testA()",
		"App/FastTests/testB()",
		"App/FastTests/testC()",
	}
	estimate := func(id string) float64 {
		if strings.Contains(id, "Slow") {
			return 100
		}
		return 1
	}

	batches, err := Plan(tests, BatchOptions{Strategy: StrategyDuration, Batches: 2, Estimate: estimate})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(batches) != 2 {
		t.Fatalf("got %d batches, want 2", len(batches))
	}

	var slowBatch Batch
	for _, b := range batches {
		for _, test := range b.Tests {
			if strings.Contains(test, "Slow") {
				slowBatch = b
			}
		}
	}
	if slowBatch.Size() != 1 {
		t.Errorf("the slow test shares a batch with %d others: %v", slowBatch.Size()-1, slowBatch.Tests)
	}
}

func TestPlanCountUsesBatchSize(t *testing.T) {
	batches, err := Plan(testIDs("LoginTests", 10), BatchOptions{Strategy: StrategyCount, BatchSize: 3})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(batches) != 4 {
		t.Fatalf("got %d batches for 10 tests at 3 each, want 4", len(batches))
	}
	for i, b := range batches[:3] {
		if b.Size() != 3 {
			t.Errorf("batch %d has %d tests, want 3", i, b.Size())
		}
	}
	if batches[3].Size() != 1 {
		t.Errorf("last batch has %d tests, want 1", batches[3].Size())
	}
}

// A batch must never carry more -only-testing arguments than one command can.
func TestPlanSplitsOversizedBatches(t *testing.T) {
	batches, err := Plan(testIDs("HugeTests", 2500), BatchOptions{Strategy: StrategyShard, Batches: 1})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(batches) != 3 {
		t.Fatalf("got %d batches, want 3 after splitting at the invocation limit", len(batches))
	}
	for _, b := range batches {
		if b.Size() > 1000 {
			t.Errorf("batch %s has %d tests, over the invocation limit", b.ID, b.Size())
		}
	}
}

func TestBatchIDsAreReadableAndUnique(t *testing.T) {
	tests := append(testIDs("LoginTests", 5), testIDs("CheckoutTests", 5)...)

	batches, err := Plan(tests, BatchOptions{Strategy: StrategyClass, Batches: 2})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}

	seen := map[string]bool{}
	for _, b := range batches {
		if seen[b.ID] {
			t.Errorf("duplicate batch id %q", b.ID)
		}
		seen[b.ID] = true

		if !strings.HasPrefix(b.ID, "batch-") {
			t.Errorf("batch id %q does not start with batch-", b.ID)
		}
		if strings.ContainsAny(b.ID, "/ .:") {
			t.Errorf("batch id %q is not safe as a file name", b.ID)
		}
		// The name says which batch it is and nothing more. A class name in
		// there would be a claim about the contents that only the class
		// strategy could keep.
		if want := fmt.Sprintf("batch-%02d", b.Index+1); b.ID != want {
			t.Errorf("batch id = %q, want %q", b.ID, want)
		}
	}
}

// Batch bundles and logs sit in one directory each, so the number is padded to
// keep a listing in plan order: batch-02 before batch-10.
func TestBatchIDsSortInPlanOrder(t *testing.T) {
	batches, err := Plan(testIDs("LoginTests", 12), BatchOptions{Strategy: StrategyCount, BatchSize: 1})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(batches) != 12 {
		t.Fatalf("got %d batches, want 12", len(batches))
	}

	ids := make([]string, 0, len(batches))
	for _, b := range batches {
		ids = append(ids, b.ID)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	if strings.Join(ids, ",") != strings.Join(sorted, ",") {
		t.Errorf("batch ids do not sort in plan order: %v", ids)
	}
	if ids[0] != "batch-01" || ids[11] != "batch-12" {
		t.Errorf("batch ids = %v, want batch-01 … batch-12", ids)
	}
}

// The hash identifies a batch by content, so the same tests are recognisable
// across runs regardless of order or position.
func TestBatchHashIsContentAddressed(t *testing.T) {
	a := hashTests([]string{"App/T/testA()", "App/T/testB()"})
	b := hashTests([]string{"App/T/testB()", "App/T/testA()"})
	c := hashTests([]string{"App/T/testA()", "App/T/testC()"})

	if a != b {
		t.Errorf("hash depends on order: %q vs %q", a, b)
	}
	if a == c {
		t.Error("different tests produced the same hash")
	}
	if len(a) != 8 {
		t.Errorf("hash %q is %d chars, want 8", a, len(a))
	}
}

func TestRetryBatchMarksTheAttempt(t *testing.T) {
	b := RetryBatch(0, 2, []string{"App/LoginTests/testA()"})

	if b.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2", b.Attempt)
	}
	if !strings.HasSuffix(b.ID, "-try2") {
		t.Errorf("ID = %q, want it to record the attempt", b.ID)
	}
}

func TestPlanRejectsBadInput(t *testing.T) {
	if _, err := Plan(nil, BatchOptions{}); err == nil {
		t.Error("Plan(nil) error = nil, want an error")
	}
	if _, err := Plan([]string{"App/T/testA()"}, BatchOptions{Strategy: "sideways"}); err == nil {
		t.Error("Plan() error = nil, want an error for an unknown strategy")
	}
}

func TestClassOf(t *testing.T) {
	tests := map[string]string{
		"App/LoginTests/testA()":       "App/LoginTests",
		"App/Outer/Inner/testA()":      "App/Outer/Inner",
		"App/LoginTests":               "App",
		"LoginTests":                   "LoginTests",
		"App/LoginTests/testA(arg:1)/": "App/LoginTests/testA(arg:1)",
	}
	for id, want := range tests {
		if got := classOf(id); got != want {
			t.Errorf("classOf(%q) = %q, want %q", id, got, want)
		}
	}
}
