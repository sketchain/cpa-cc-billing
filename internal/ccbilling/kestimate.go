package ccbilling

import (
	"sort"
	"time"
)

// KBucket sums an account's k evidence over one hour.
type KBucket struct {
	At     time.Time `json:"at"`
	Gain5h float64   `json:"gain_5h"`
	Gain7d float64   `json:"gain_7d"`
}

const kBucketWidth = time.Hour

// BucketEvidence sums evidence into hourly buckets, oldest first.
func BucketEvidence(evidence []KEvidence) []KBucket {
	sums := map[time.Time]*KBucket{}
	for _, item := range evidence {
		at := item.At.UTC().Truncate(kBucketWidth)
		bucket := sums[at]
		if bucket == nil {
			bucket = &KBucket{At: at}
			sums[at] = bucket
		}
		bucket.Gain5h += item.Gain5h
		bucket.Gain7d += item.Gain7d
	}
	buckets := make([]KBucket, 0, len(sums))
	for _, bucket := range sums {
		buckets = append(buckets, *bucket)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].At.Before(buckets[j].At) })
	return buckets
}

// mergeBuckets adds new buckets into a sorted list.
func mergeBuckets(buckets, added []KBucket) []KBucket {
	index := make(map[time.Time]int, len(buckets))
	for i, bucket := range buckets {
		index[bucket.At] = i
	}
	for _, bucket := range added {
		if i, ok := index[bucket.At]; ok {
			buckets[i].Gain5h += bucket.Gain5h
			buckets[i].Gain7d += bucket.Gain7d
			continue
		}
		index[bucket.At] = len(buckets)
		buckets = append(buckets, bucket)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].At.Before(buckets[j].At) })
	return buckets
}

// KGroup is the k evidence of one age group.
type KGroup struct {
	Gain5h float64 `json:"gain_5h"`
	Gain7d float64 `json:"gain_7d"`
	K      float64 `json:"k"`
	// Valid means the group's 7-day gain reached the configured minimum.
	Valid bool `json:"valid"`
}

func (g *KGroup) finish(params Params) {
	if g.Gain7d > 0 && g.Gain5h > 0 {
		g.K = g.Gain5h / g.Gain7d
	}
	g.Valid = g.K > 0 && g.Gain7d >= params.KMinWeeklyPoints-gainEpsilon
}

// k bases.
const (
	KBasisBlend  = "blend"
	KBasisRecent = "recent"
	KBasisOld    = "old"
	KBasisTotal  = "total"
	KBasisNone   = "none"
)

// KEstimate is an account's estimated k, time-weighted like the totals.
type KEstimate struct {
	Recent KGroup   `json:"recent"`
	Old    KGroup   `json:"old"`
	Total  KGroup   `json:"total"`
	K      *float64 `json:"k,omitempty"`
	Basis  string   `json:"basis"`
}

// EstimateK splits the buckets at the recent period. Each group's k is its
// 5-hour gain over its 7-day gain and counts once its 7-day gain reaches the
// minimum. Two valid groups combine by the recent share; one valid group is
// used alone; otherwise the combined ratio counts if the total is enough.
func EstimateK(buckets []KBucket, params Params, now time.Time) KEstimate {
	cutoff := now.Add(-params.recent())
	var result KEstimate
	for _, bucket := range buckets {
		group := &result.Recent
		if bucket.At.Add(kBucketWidth).Before(cutoff) {
			group = &result.Old
		}
		group.Gain5h += bucket.Gain5h
		group.Gain7d += bucket.Gain7d
	}
	result.Total = KGroup{Gain5h: result.Recent.Gain5h + result.Old.Gain5h, Gain7d: result.Recent.Gain7d + result.Old.Gain7d}
	result.Recent.finish(params)
	result.Old.finish(params)
	result.Total.finish(params)
	var k float64
	switch {
	case result.Recent.Valid && result.Old.Valid:
		k, result.Basis = params.RecentShare*result.Recent.K+(1-params.RecentShare)*result.Old.K, KBasisBlend
	case result.Recent.Valid:
		k, result.Basis = result.Recent.K, KBasisRecent
	case result.Old.Valid:
		k, result.Basis = result.Old.K, KBasisOld
	case result.Total.Valid:
		k, result.Basis = result.Total.K, KBasisTotal
	default:
		result.Basis = KBasisNone
		return result
	}
	result.K = &k
	return result
}
