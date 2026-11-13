package main

// mq_player reports TrackDuration values of 2-6 seconds while it is still
// opening a network stream. Those values are not episode lengths: if they
// replace the RSS duration the status loop believes the episode has already
// ended, deletes the breakpoint and stops the session. Keep every duration
// decision in one place so the poll loop, the session clock and the breakpoint
// store agree on what a trustworthy duration looks like.

// minimumPlausibleDuration is the shortest renderer-reported duration we are
// willing to trust. RSS-provided durations are trusted as-is (they do not pass
// through sanitizeDuration), so genuinely short clips still work.
const minimumPlausibleDuration = 30

// sanitizeDuration returns the duration that should be kept, given the value we
// already trust (usually from the RSS feed) and a freshly reported value.
// A candidate is accepted only when it is plausible on its own, and it may not
// shrink a trusted duration by more than a quarter.
func sanitizeDuration(known, candidate int) int {
	if candidate <= 0 {
		return known
	}
	if candidate < minimumPlausibleDuration {
		return known
	}
	if known >= minimumPlausibleDuration && candidate*4 < known*3 {
		return known
	}
	return candidate
}

// durationReachedEnd reports whether pos is close enough to dur to consider the
// episode finished. An unknown or implausible duration never means the episode
// ended; the audio proxy's EOF flag handles the remaining cases.
func durationReachedEnd(pos, dur int) bool {
	if dur < minimumPlausibleDuration {
		return false
	}
	return pos >= dur-15
}
