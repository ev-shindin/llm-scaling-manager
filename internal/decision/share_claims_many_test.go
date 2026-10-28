package decision

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Several models waking at once race for the transfers still releasing: the
// scale-from-zero loop runs one claim per wake, concurrently with the others.
// Each transfer goes to exactly one wake, however many claim it, and a wake
// that finds none left is told so.
func TestShareClaimStoreManyWakesOneWinnerPerTransfer(t *testing.T) {
	t0 := time.Unix(0, 0)
	cluster := ShareGroupKey("", "A100")
	for _, tc := range []struct{ transfers, wakes int }{
		{1, 2}, {1, 3}, {2, 3}, {1, 8}, {3, 8},
	} {
		t.Run(fmt.Sprintf("%d transfers %d wakes", tc.transfers, tc.wakes), func(t *testing.T) {
			for round := range 20 { // goroutine order differs between rounds
				entries := make([]ShareClaimable, 0, tc.transfers)
				for i := range tc.transfers {
					entries = append(entries, ShareClaimable{ID: fmt.Sprintf("t%d", i), ReceiverZ: -0.2,
						DonorGPUs: 8, DonorPodGPUs: []int{8}})
				}
				s := claimStore(map[string][]ShareClaimable{cluster: entries}, t0)
				outcomes := make([]string, tc.wakes)
				ids := make([]string, tc.wakes)
				var start, done sync.WaitGroup
				start.Add(1)
				for w := range tc.wakes {
					done.Add(1)
					go func() {
						defer done.Done()
						start.Wait()
						c, outcome := s.Claim("", "A100", []int{8}, 8, -1,
							fmt.Sprintf("ns/m%d", w), fmt.Sprintf("ns/w%d", w), t0)
						outcomes[w], ids[w] = outcome, c.ID
					}()
				}
				start.Done()
				done.Wait()
				won := map[string]int{}
				for w, o := range outcomes {
					switch o {
					case ShareClaimRedirected:
						won[ids[w]]++
					case ShareClaimNoneReleasing:
					default:
						t.Fatalf("round %d: wake %d: outcome %q, want redirected or none-releasing", round, w, o)
					}
				}
				if len(won) != tc.transfers {
					t.Fatalf("round %d: %d transfers claimed, want all %d: %v", round, len(won), tc.transfers, outcomes)
				}
				for id, n := range won {
					if n != 1 {
						t.Fatalf("round %d: transfer %s claimed by %d wakes, want exactly one", round, id, n)
					}
				}
				if got := s.Take("", "A100"); len(got) != tc.transfers {
					t.Fatalf("round %d: %d claims to take, want %d", round, len(got), tc.transfers)
				}
			}
		})
	}
}
