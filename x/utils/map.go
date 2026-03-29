package utils

func MergeMaps(map1, map2 map[string]string) map[string]string {
	mergedMap := make(map[string]string)

	for key, value := range map1 {
		mergedMap[key] = value
	}

	for key, value := range map2 {
		mergedMap[key] = value
	}

	return mergedMap
}
