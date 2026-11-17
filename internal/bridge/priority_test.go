package bridge

import "testing"

// The bands hold their order: a listener waiting on an episode first, the
// scheduled work under anything a person asked for, and Back up everything
// under the scheduled podcast fetches, so a bulk backup in the Bridge's one
// download slot never holds back new episodes (bridge#46).
func TestPriorityBands(t *testing.T) {
	t.Parallel()
	bands := []int{PriorityPlaying, PriorityDownload, PriorityListener, PriorityAsked, PriorityScheduled, PriorityBulk}
	for i := 1; i < len(bands); i++ {
		if bands[i] >= bands[i-1] {
			t.Fatalf("band %d (%d) is not under band %d (%d)", i, bands[i], i-1, bands[i-1])
		}
	}
	if (NewJob{}).Priority != PriorityAsked {
		t.Fatal("a job queued with no priority is not at PriorityAsked")
	}
}
