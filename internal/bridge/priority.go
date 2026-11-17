package bridge

// Job priorities, the bands the server queues at: a claim takes the highest
// first, then the oldest (Claim). One list, so a band's place against another
// (a bulk backup behind a show's new episodes in the Bridge's one download
// slot) is read here rather than across the packages that queue.
const (
	// PriorityPlaying: an episode a listener is waiting to play.
	PriorityPlaying = 100
	// PriorityDownload: an episode a listener asked to download.
	PriorityDownload = 20
	// PriorityListener: a listener's follow, and the episode after the one
	// they play, fetched ahead.
	PriorityListener = 10
	// PriorityAsked: what an admin asks for in the console (NewJob's zero
	// value), a title backed up one by one among them.
	PriorityAsked = 0
	// PriorityScheduled: the scheduled polls and fetches (podcasts, Audible,
	// the Audible sync) and the backups a policy queues after a poll.
	PriorityScheduled = -10
	// PriorityBulk: Back up everything and a show's retention, under
	// everything else.
	PriorityBulk = -20
)
