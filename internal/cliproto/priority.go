package cliproto

// Priority tiers carried on the envelope: 1=high, 2=medium, 3=low.
// 0 means unset (legacy envelopes, senders that didn't ask) and is
// treated as medium by readers.
const (
	PriorityHigh   = 1
	PriorityMedium = 2
	PriorityLow    = 3
)

// EffectivePriority maps a raw envelope priority to the tier readers
// sort on. The single home of the "0 (unset) and anything out-of-range
// mean medium" rule: legacy envelopes decode priority to 0 and must
// interleave exactly like explicit mediums, and a foreign publisher
// writing garbage straight onto NATS (bypassing the daemon's send
// validation) cannot mint a super-priority tier or break the sort.
func EffectivePriority(p int) int {
	if p < PriorityHigh || p > PriorityLow {
		return PriorityMedium
	}
	return p
}
