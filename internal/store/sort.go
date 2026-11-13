package store

import "sort"

func sortByScoreDesc(hits []Hit) {
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
}
