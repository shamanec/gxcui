package executor

import (
	"strings"
	"testing"
)

func TestBuildTree(t *testing.T) {
	roots := BuildTree([]string{
		"AppUITests/LoginTests/testA()",
		"AppUITests/LoginTests/testB()",
		"AppUITests/CheckoutTests/testC()",
		"OtherUITests/SmokeTests/testD()",
	})

	if len(roots) != 2 {
		t.Fatalf("got %d targets, want 2", len(roots))
	}
	// Order of first appearance is preserved at every level.
	if roots[0].Name != "AppUITests" || roots[1].Name != "OtherUITests" {
		t.Fatalf("targets = %q, %q; want AppUITests, OtherUITests", roots[0].Name, roots[1].Name)
	}

	classes := roots[0].Children
	if len(classes) != 2 || classes[0].Name != "LoginTests" || classes[1].Name != "CheckoutTests" {
		t.Fatalf("classes = %v, want LoginTests then CheckoutTests", names(classes))
	}
	if got := names(classes[0].Children); strings.Join(got, ",") != "testA(),testB()" {
		t.Errorf("LoginTests children = %v, want testA(), testB()", got)
	}
	if !classes[0].Children[0].Leaf() {
		t.Error("a test node should be a leaf")
	}
	if classes[0].Leaf() {
		t.Error("a class with tests should not be a leaf")
	}
}

func TestBuildTreeHandlesExtraLevels(t *testing.T) {
	// Nested suites produce a deeper tree rather than a parse failure.
	roots := BuildTree([]string{"Target/Outer/Inner/test()"})

	node := roots[0]
	for _, want := range []string{"Target", "Outer", "Inner", "test()"} {
		if node.Name != want {
			t.Fatalf("level name = %q, want %q", node.Name, want)
		}
		if len(node.Children) > 0 {
			node = node.Children[0]
		}
	}
}

func TestBuildTreeEmpty(t *testing.T) {
	if got := BuildTree(nil); got != nil {
		t.Errorf("BuildTree(nil) = %v, want nil", got)
	}
}

func names(nodes []*TreeNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}
