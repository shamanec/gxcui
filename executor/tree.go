package executor

import "strings"

// TreeNode is one level of a test identifier tree: a target, a class, or a test.
type TreeNode struct {
	Name     string      `json:"name"`
	Children []*TreeNode `json:"children,omitempty"`
}

// Leaf reports whether the node is a test rather than a grouping level.
func (n *TreeNode) Leaf() bool { return len(n.Children) == 0 }

// BuildTree groups flat test identifiers into a tree by splitting on "/".
//
// The tree is derived from identifiers rather than from xcodebuild's
// hierarchical enumeration, so it always reflects exactly the tests that would
// run — filters included — and it costs no extra enumeration run. Identifiers
// with more levels than the usual Target/Class/method (nested Swift Testing
// suites, for instance) simply produce a deeper tree.
//
// Order of first appearance is preserved at every level.
func BuildTree(identifiers []string) []*TreeNode {
	var roots []*TreeNode
	index := map[string]*TreeNode{}

	for _, id := range identifiers {
		var path string
		children := &roots
		for _, part := range strings.Split(id, "/") {
			if path == "" {
				path = part
			} else {
				path += "/" + part
			}

			node, ok := index[path]
			if !ok {
				node = &TreeNode{Name: part}
				index[path] = node
				*children = append(*children, node)
			}
			children = &node.Children
		}
	}
	return roots
}
