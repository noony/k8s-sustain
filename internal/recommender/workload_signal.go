package recommender

// EffectiveReplicas returns the replica count to use as a divisor when converting
// workload-level totals into per-pod values. medianReplicas is the median count
// reported by Prometheus over the recommendation window. When that is zero or
// negative (e.g. KEDA scale-to-zero, missing samples), we fall back to
// max(minReplicas, 1) so we never divide by zero.
func EffectiveReplicas(medianReplicas float64, minReplicas int32) float64 {
	if medianReplicas > 0 {
		return medianReplicas
	}
	if minReplicas > 0 {
		return float64(minReplicas)
	}
	return 1
}

// PerPodFromTotal divides a workload-total resource value by the effective
// replica count. When replicas is zero (defensive — callers should pass a
// value already cleaned by EffectiveReplicas), it returns total unchanged.
func PerPodFromTotal(total, replicas float64) float64 {
	if replicas <= 0 {
		return total
	}
	return total / replicas
}

// ApplyFloor returns max(value, floor). A zero floor is treated as "no floor".
// Used to enforce a per-pod p95 minimum on top of workload-derived recommendations,
// protecting against load imbalance where one replica runs hotter than the average.
func ApplyFloor(value, floor float64) float64 {
	if floor <= 0 || value >= floor {
		return value
	}
	return floor
}
