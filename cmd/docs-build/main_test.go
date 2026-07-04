package main

import "testing"

// TestRankSlug_KnownSlugsReturnIndex verifies that each slug in the order
// table returns its exact index, and that slug ordering is strictly monotone.
// If someone inserts a new entry at the wrong position the comparisons here
// catch it immediately.
func TestRankSlug_KnownSlugsReturnIndex(t *testing.T) {
	for i, slug := range order {
		got := rankSlug(slug)
		if got != i {
			t.Errorf("rankSlug(%q) = %d, want %d", slug, got, i)
		}
	}
	// Consecutive entries must be strictly ordered so the sort is deterministic.
	for i := 0; i+1 < len(order); i++ {
		if rankSlug(order[i]) >= rankSlug(order[i+1]) {
			t.Errorf("order[%d]=%q (rank %d) not strictly < order[%d]=%q (rank %d)",
				i, order[i], rankSlug(order[i]),
				i+1, order[i+1], rankSlug(order[i+1]))
		}
	}
}

// TestRankSlug_UnknownSlugRanksLast verifies that a slug not present in the
// order table ranks after all known slugs (returns len(order)+1). This is the
// mechanism that pushes unlisted pages to the bottom of the sidebar.
func TestRankSlug_UnknownSlugRanksLast(t *testing.T) {
	unknown := "zz-this-page-does-not-exist-in-order"
	got := rankSlug(unknown)
	want := len(order) + 1
	if got != want {
		t.Errorf("rankSlug(%q) = %d, want %d (len(order)+1)", unknown, got, want)
	}
	// Must be strictly after every known slug.
	for _, slug := range order {
		if rankSlug(slug) >= got {
			t.Errorf("known slug %q (rank %d) should be < unknown rank %d", slug, rankSlug(slug), got)
		}
	}
}

// TestRankSlug_FirstBeforeLast is a coarse sanity check that the first and
// last pinned slugs maintain their relative order in the sidebar.
func TestRankSlug_FirstBeforeLast(t *testing.T) {
	first, last := order[0], order[len(order)-1]
	if rankSlug(first) >= rankSlug(last) {
		t.Errorf("first slug %q (rank %d) should be before last slug %q (rank %d)",
			first, rankSlug(first), last, rankSlug(last))
	}
}
