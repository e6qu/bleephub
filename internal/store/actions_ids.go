package store

func JsonSafePositiveID(hash uint64) int64 {
	id := hash & MaxJSONSafeInteger
	if id == 0 {
		id = 1
	}
	return int64(id)
}
