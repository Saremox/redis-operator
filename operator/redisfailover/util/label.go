package util

// MergeLabels returns a new map with all the labels. A later map wins on a
// duplicate key.
func MergeLabels(allLabels ...map[string]string) map[string]string {
	res := map[string]string{}

	for _, labels := range allLabels {
		for k, v := range labels {
			res[k] = v
		}
	}
	return res
}

// MergeAnnotations returns a new map with all the annotations. A later map
// wins on a duplicate key.
func MergeAnnotations(allMergeAnnotations ...map[string]string) map[string]string {
	res := map[string]string{}

	for _, annotations := range allMergeAnnotations {
		for k, v := range annotations {
			res[k] = v
		}
	}
	return res
}
