package toxicity

import "math"

// vpin estimates VPIN: trades are accumulated into equal-volume buckets; each
// completed bucket contributes its order-flow imbalance |buy − sell| / bucketVol
// in [0,1], and VPIN is the mean imbalance over the last maxBuckets buckets.
// High VPIN ⇒ persistent one-sided (informed) flow ⇒ toxic.
type vpin struct {
	bucketVol  float64
	maxBuckets int
	curBuy     float64
	curSell    float64
	buckets    []float64
}

func newVPIN(bucketVol float64, maxBuckets int) *vpin {
	if maxBuckets < 1 {
		maxBuckets = 1
	}
	return &vpin{bucketVol: bucketVol, maxBuckets: maxBuckets}
}

// observe adds a trade of size qty to the current bucket, closing buckets as
// they fill. To keep the estimator simple and deterministic, a trade that
// overflows the bucket boundary still closes the bucket at that point and the
// remainder seeds the next bucket on the same side.
func (v *vpin) observe(qty float64, buy bool) {
	if v.bucketVol <= 0 || qty <= 0 {
		return
	}
	for qty > 0 {
		room := v.bucketVol - (v.curBuy + v.curSell)
		take := qty
		if take > room {
			take = room
		}
		if buy {
			v.curBuy += take
		} else {
			v.curSell += take
		}
		qty -= take
		if v.curBuy+v.curSell >= v.bucketVol {
			imb := math.Abs(v.curBuy-v.curSell) / (v.curBuy + v.curSell)
			v.buckets = ringAppend(v.buckets, imb, v.maxBuckets)
			v.curBuy, v.curSell = 0, 0
		}
	}
}

// value returns the mean bucket imbalance in [0,1] (0 until the first bucket
// completes).
func (v *vpin) value() float64 {
	if len(v.buckets) == 0 {
		return 0
	}
	var sum float64
	for _, b := range v.buckets {
		sum += b
	}
	return sum / float64(len(v.buckets))
}
