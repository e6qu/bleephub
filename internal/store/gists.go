package store

import "sort"

func SortHistory(history []*GistHistory) {
	sort.Slice(history, func(i, j int) bool {
		return history[i].CommittedAt.After(history[j].CommittedAt)
	})
}
