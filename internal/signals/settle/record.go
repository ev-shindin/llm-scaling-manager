package settle

// KindObserve is the learner recording's event for the engine's Observe of
// one model's cycle (docs/proposals/analyzer-learner.md, 2.4). It records
// the model and, as the event's clock, the instant the engine observed at;
// the rows are the ones the model's next Analyze event records, so they are
// not recorded twice. A replay observes its own tracker with them at that
// instant.
const KindObserve = "settle.Observe"

// ObserveEvent is KindObserve's data.
type ObserveEvent struct {
	Namespace string
	ModelID   string
}
